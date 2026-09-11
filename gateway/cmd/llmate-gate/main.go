// Command llmate-gate 是 LLMate Gate 的入口：装载配置、组装依赖、启动网关（契约 §4）。
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"gateway/debug"
	"gateway/internal/audit"
	"gateway/internal/cache"
	"gateway/internal/circuit"
	"gateway/internal/config"
	"gateway/internal/detector"
	"gateway/internal/metrics"
	"gateway/internal/pipeline"
	"gateway/internal/policy"
	"gateway/internal/proxy"
	"gateway/internal/replacer"
	"gateway/internal/server"
	"gateway/internal/simulator"
	"gateway/internal/vault"
)

// version 由 release.sh 通过 -ldflags "-X main.version=$VERSION" 注入。
// 留默认 "dev" 以便 go run / 开发构建不出错。
var version = "dev"

func main() {
	var (
		configPath  string
		noDebug     bool
		listen      string
		showVersion bool
	)
	flag.StringVar(&configPath, "config", "", "path to config.yaml (empty=defaults)")
	flag.BoolVar(&noDebug, "no-debug", false, "disable embedded debug panel (/_debug, /ws/events, /_api/*)")
	flag.StringVar(&listen, "listen", "", "override gateway.listen (e.g. :8400)")
	flag.BoolVar(&showVersion, "version", false, "print version and exit (also -V)")
	flag.Parse()

	// --version / -version：打印注入的版本号后干净退出，供 scoop post_install 等外部调用方校验。
	if showVersion {
		fmt.Printf("llmate-gate %s\n", version)
		return
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	if noDebug {
		cfg.Gateway.Debug = false
	}
	if listen != "" {
		cfg.Gateway.Listen = listen
	}
	log.Printf("[llmate-gate] %s (version=%s)", cfg.String(), version)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Vault：内存驻留 + 可选加密落盘；密钥随机生成不落盘。
	v, err := vault.NewMemVault(cfg.Vault.RequestTTL, nil, cfg.Vault.Persist, cfg.Vault.Path)
	if err != nil {
		log.Fatalf("vault init error: %v", err)
	}
	defer v.Close()
	go v.StartSweeper(ctx, time.Minute)

	// 检测引擎：regex（内置）或 pii-engineer（sidecar）。
	opts := []detector.Option{detector.WithThresholds(cfg.Detection.Thresholds)}
	var det detector.Client
	switch cfg.Detection.Engine {
	case "pii-engineer":
		sc := cfg.Detection.Sidecar
		det = detector.NewPIIEngineerClient(sc.Endpoint, sc.Healthz, 500*time.Millisecond, opts...)
	default:
		det = detector.NewRegexEngine(opts...)
	}

	// 熔断：连续失败进入半开探测，配合 fail-closed。
	breaker := circuit.New(5, 10*time.Second, func(open bool) {
		if open {
			log.Printf("[llmate-gate] detector circuit OPEN (fail-closed active)")
		} else {
			log.Printf("[llmate-gate] detector circuit CLOSED (recovered)")
		}
	})
	guarded := circuit.NewGuardedClient(det, breaker)

	// 替换器 + 仿真配置（Replacer 接口含 SetStrategy，支持规则热加载）。
	sessionKey := deriveSessionKey()

	// per-type fate 策略（Phase 2 阶段 2）：配置覆盖 + 历史 irreversible 列表并入。
	pol, err := policy.New(cfg.Replacement.PerTypeFate)
	if err != nil {
		log.Fatalf("policy config error: %v", err)
	}
	pol = pol.WithIrreversible(cfg.Replacement.Irreversible)

	repl := replacer.New(replacer.Config{
		Strategy:     cfg.Replacement.Strategy,
		Irreversible: cfg.Replacement.Irreversible,
		Simulate: simulator.SimulateZHConfig{
			PersonName: cfg.Replacement.SimulateZH.PersonName,
			Phone:      cfg.Replacement.SimulateZH.Phone,
			IDCard:     cfg.Replacement.SimulateZH.IDCard,
			BankCard:   cfg.Replacement.SimulateZH.BankCard,
		},
		SessionKey: sessionKey,
		Policy:     pol,
	}, v)

	// 检测缓存。
	var dc cache.Cache
	if cfg.Detection.Cache.Enabled {
		dc = cache.NewLRU(cfg.Detection.Cache.MaxEntries, cfg.Detection.Cache.TTL, cfg.Detection.Cache.BindConversation)
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					dc.Sweep()
				}
			}
		}()
	}

	// Merkle 增量检测缓存：绑定 conversation_id，只扫新增 turn（Phase 2 阶段 2）。
	var merkle *cache.MerkleCache
	if cfg.Detection.Cache.Enabled && cfg.Detection.Cache.BindConversation {
		merkle = cache.NewMerkle(cfg.Detection.Cache.TTL)
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					merkle.Sweep()
				}
			}
		}()
	}

	// 审计日志。
	alog, err := audit.NewLogger(cfg.Audit.Path, cfg.Audit.Enabled, cfg.Audit.LogPII)
	if err != nil {
		log.Fatalf("audit init error: %v", err)
	}
	defer alog.Close()

	// 指标。
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	metricHandler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})

	// 内嵌调试面板（任务 1.5）：Hub + Store（受 --no-debug / debug=false 门控）。
	var (
		debugStore *debug.TrafficRecordStore
		debugHub   *debug.Hub
	)
	if cfg.Gateway.Debug {
		debugStore = debug.NewTrafficStore(200)
		debugHub = debug.NewHub(debugStore)
	}
	publisher := toPublisher(debugHub)

	// 编排器（Publisher 即 debugHub 或 NopPublisher）。
	proc := pipeline.New(pipeline.Config{
		Detector:   guarded,
		Replacer:   repl,
		Vault:      v,
		Cache:      dc,
		Audit:      alog,
		Metrics:    m,
		Publisher:  publisher,
		UseCache:   cfg.Detection.Cache.Enabled,
		FailClosed: cfg.Policy.FailClosed,
	})

	// 代理。
	up, err := url.Parse(cfg.Gateway.Upstream)
	if err != nil {
		log.Fatalf("invalid upstream url: %v", err)
	}
	px := proxy.New(proc, up, cfg.Gateway.UpstreamAPIKey, "2023-06-01", m, cfg.Audit.LogPII, merkle)

	// 服务。
	srv := server.New(server.Options{
		Proxy:     px,
		Config:    cfg,
		Metrics:   m,
		AuthToken: cfg.Gateway.AuthToken,
		Health:    guarded.Health,
	})
	srv.SetMetricHandler(metricHandler)

	// 挂载调试面板路由（debug=true 时；--no-debug 时 cfg.Gateway.Debug=false 已生效）。
	if cfg.Gateway.Debug {
		dh := debug.NewHandler(cfg, guarded, repl, debugHub, debugStore, func(strategy string) error {
			return repl.SetStrategy(strategy)
		}, alog)
		dh.Mount(srv.Mux())
		log.Printf("[llmate-gate] debug panel mounted at %s/_debug", cfg.Gateway.Listen)
	} else {
		log.Printf("[llmate-gate] debug panel disabled (--no-debug or config.debug=false)")
	}

	// 信号：优雅退出。
	// 顺序很重要：必须**先 cancel() 再 Close()。cancel() 会让 HTTP server 立即开始
	// 优雅关闭并最终让 Start 返回；若把 Close() 放在前面，一旦它阻塞，cancel() 就
	// 永远执行不到 —— 进程既不退出也不释放端口，等待它的 CI 脚本会一直卡到超时。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Printf("[llmate-gate] shutting down")
		cancel()
		if debugHub != nil {
			debugHub.Close()
		}
		// 兜底：优雅关闭若卡住，第二个信号直接强制退出，避免进程悬挂。
		<-sig
		log.Printf("[llmate-gate] forced exit on second signal")
		os.Exit(1)
	}()

	log.Printf("[llmate-gate] listening on %s (debug=%v)", cfg.Gateway.Listen, cfg.Gateway.Debug)
	if err := srv.Start(ctx, cfg.Gateway.Listen); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// deriveSessionKey 派生会话级仿真密钥（重启即失效；可用 GATEWAY_SESSION_KEY 固定）。
func deriveSessionKey() []byte {
	if k := os.Getenv("GATEWAY_SESSION_KEY"); k != "" {
		return []byte(k)
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		log.Printf("[llmate-gate] session key fallback to time seed: %v", err)
		return []byte("llmate-gate-default-session-key-32b")
	}
	return k
}

// toPublisher 把可选的 debug.Hub 适配为 pipeline.EventPublisher；hub==nil 返回 NopPublisher。
func toPublisher(h *debug.Hub) pipeline.EventPublisher {
	if h == nil {
		return pipeline.NopPublisher{}
	}
	return h
}