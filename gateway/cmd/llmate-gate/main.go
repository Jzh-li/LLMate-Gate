// Command llmate-gate 是 LLMate Gate 的入口：装载配置、组装依赖、启动网关（契约 §4）。
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"log"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"gateway/internal/audit"
	"gateway/internal/cache"
	"gateway/internal/circuit"
	"gateway/internal/config"
	"gateway/internal/detector"
	"gateway/internal/metrics"
	"gateway/internal/pipeline"
	"gateway/internal/proxy"
	"gateway/internal/replacer"
	"gateway/internal/server"
	"gateway/internal/simulator"
	"gateway/internal/vault"
)

func main() {
	var (
		configPath string
		noDebug    bool
		listen     string
	)
	flag.StringVar(&configPath, "config", "", "path to config.yaml (empty=defaults)")
	flag.BoolVar(&noDebug, "no-debug", false, "disable embedded debug panel (/_debug, /ws/events, /_api/*)")
	flag.StringVar(&listen, "listen", "", "override gateway.listen (e.g. :8400)")
	flag.Parse()

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
	log.Printf("[llmate-gate] %s", cfg.String())

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

	// 替换器 + 仿真配置。
	sessionKey := deriveSessionKey()
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

	// 编排器（Publisher 在调试面板就绪后注入，见 internal/debug）。
	proc := pipeline.New(pipeline.Config{
		Detector:   guarded,
		Replacer:   repl,
		Vault:      v,
		Cache:      dc,
		Audit:      alog,
		Metrics:    m,
		Publisher:  nil,
		UseCache:   cfg.Detection.Cache.Enabled,
		FailClosed: cfg.Policy.FailClosed,
	})

	// 代理。
	up, err := url.Parse(cfg.Gateway.Upstream)
	if err != nil {
		log.Fatalf("invalid upstream url: %v", err)
	}
	px := proxy.New(proc, up, cfg.Gateway.UpstreamAPIKey, "2023-06-01", m, cfg.Audit.LogPII)

	// 服务。
	srv := server.New(server.Options{
		Proxy:     px,
		Config:    cfg,
		Metrics:   m,
		AuthToken: cfg.Gateway.AuthToken,
		Health:    guarded.Health,
	})
	srv.SetMetricHandler(metricHandler)

	// TODO(task4): 若 cfg.Gateway.Debug，挂载 internal/debug 路由到 srv.Mux()。

	// 信号：优雅退出。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Printf("[llmate-gate] shutting down")
		cancel()
	}()

	log.Printf("[llmate-gate] listening on %s (debug=%v)", cfg.Gateway.Listen, cfg.Gateway.Debug)
	if err := srv.Start(cfg.Gateway.Listen); err != nil {
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
