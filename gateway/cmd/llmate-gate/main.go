// Command llmate-gate 是 LLMate Gate 的入口：装载配置、组装依赖、启动网关（契约 §4）。
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
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
	"gateway/internal/registry"
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

	// 访问令牌：显式配置 > 上次自动生成并落盘的 > 新生成并落盘。
	// 解析放在启动最前面：拿到令牌才算「这次启动的安全状态已确定」，后面的
	// 告警与监听才有意义。
	authTok, tokenGenerated, err := cfg.ResolveAuthToken()
	if err != nil {
		log.Fatalf("auth token error: %v", err)
	}

	log.Printf("[llmate-gate] %s (version=%s)", cfg.String(), version)
	logSecurityWarnings(cfg, authTok, tokenGenerated)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Vault：仅内存驻留；密钥随机生成、不落盘（见 vault 包注释）。
	v, err := vault.NewMemVault(cfg.Vault.RequestTTL, nil)
	if err != nil {
		log.Fatalf("vault init error: %v", err)
	}
	defer func() { _ = v.Close() }()
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

	// 登记表：用户自报真实 PII 值，检测层精确匹配补召回。
	//
	// 加载失败一律 fatal：带着「半张登记表」启动是最坏的结局——用户以为某个值
	// 已经被保护了，其实没有。文件不存在不算错（首次运行、值都由面板录入）。
	piiReg := loadRegistry(cfg)
	det = registry.Wrap(det, piiReg)

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
			Dictionary: cfg.Replacement.SimulateZH.Dictionary,
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

	// 登记表一变，两层检测缓存都得清：缓存键只由文本哈希 / 段哈希构成，看不出
	// 检测的输入侧已经变了。不清的话「刚补登一个值、重发同一句话」会命中旧结果，
	// 而这恰恰是这个功能最主要的用法。
	if piiReg != nil {
		piiReg.OnChange(func() {
			if dc != nil {
				dc.Flush()
			}
			if merkle != nil {
				merkle.Clear()
			}
			log.Printf("[llmate-gate] registry changed (%d entries) → detection caches flushed", piiReg.Len())
		})
	}

	// 审计日志。
	alog, err := audit.NewLoggerRotating(cfg.Audit.Path, cfg.Audit.Enabled, cfg.Audit.LogPII,
		int64(cfg.Audit.MaxSizeMB)<<20, cfg.Audit.MaxBackups)
	if err != nil {
		log.Fatalf("audit init error: %v", err)
	}
	defer func() { _ = alog.Close() }()

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

	// 代理：默认 OpenAI 上游 + 可选的协议路由上游（config 驱动各家厂商适配）。
	//
	// 合并规则（gateway.upstream 作默认、upstreams[] 同协议覆盖）只在
	// config.EffectiveUpstreams 里实现一份；启动日志打印的也是同一份结果，
	// 于是「日志说连哪里」与「实际连哪里」不会再分叉。
	var openaiUp, anthropicUp *proxy.Upstream
	for _, u := range cfg.EffectiveUpstreams() {
		bu, err := url.Parse(u.BaseURL)
		if err != nil {
			log.Fatalf("invalid upstream url (%s=%s): %v", u.Protocol, u.BaseURL, err)
		}
		target := &proxy.Upstream{
			URL: bu, APIKey: u.APIKey, APIVersion: u.APIVersion, PathPrefix: u.PathPrefix,
		}
		if u.Protocol == "anthropic" {
			anthropicUp = target
		} else {
			openaiUp = target
		}
	}
	px := proxy.New(proc, openaiUp, anthropicUp, m, cfg.Audit.LogPII, merkle)

	// 服务。
	srv := server.New(server.Options{
		Proxy:            px,
		Config:           cfg,
		Metrics:          m,
		AuthToken:        authTok,
		ControlAuthToken: cfg.Gateway.ControlAuthToken,
		Health:           guarded.Health,
	})
	srv.SetMetricHandler(metricHandler)

	// 挂载调试面板路由（debug=true 时；--no-debug 时 cfg.Gateway.Debug=false 已生效）。
	//
	// 面板挂到控制面 mux：它的数据端点（流量详情、Playground、登记表）都能看到明文
	// PII，与 /v1/privacy/* 属于同一种能力，不该只需要数据面令牌。静态壳保持匿名——
	// 壳里没有请求数据，而且没有它就没有地方输入令牌。
	if cfg.Gateway.Debug {
		dh := debug.NewHandler(debug.Options{
			Config:      cfg,
			Detector:    guarded,
			Replacer:    repl,
			Hub:         debugHub,
			Store:       debugStore,
			RuleHook:    repl.SetStrategy,
			AuditSource: alog,
			Registry:    piiReg,
		})
		dh.Mount(srv.ControlMux())
		srv.AdoptDebugRoutes()
		if authTok != "" {
			log.Printf("[llmate-gate] debug panel at %s/_debug — it requires control-plane credentials "+
				"(it shows request plaintext). Open %s/_debug?token=<token> once to set the panel cookie; "+
				"the token is the control-plane token (see gateway.auth_token / auth_token_file).",
				cfg.Gateway.Listen, cfg.Gateway.Listen)
		} else {
			log.Printf("[llmate-gate] debug panel mounted at %s/_debug (no authentication configured, loopback-only)",
				cfg.Gateway.Listen)
		}
		// 面板的全部网络防线是「对端地址是不是回环」。这在同机反向代理后面会失效：
		// 代理把 RemoteAddr 变成 127.0.0.1，于是面板跟着网关一起被暴露出去。
		// 绑定非回环地址时明确说出来，而不是等用户自己发现。
		if !isLoopbackListen(cfg.Gateway.Listen) {
			log.Printf("[llmate-gate] SECURITY WARNING: debug panel enabled while listen=%q is not "+
				"loopback-bound. The panel exposes request plaintext and the PII registry; its loopback "+
				"check cannot see past a same-host reverse proxy. Bind to 127.0.0.1 or use --no-debug.",
				cfg.Gateway.Listen)
		}
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

// logSecurityWarnings 在启动期显式陈述当前的安全状态。
//
// 存在的理由：多数暴露面不是「配置写错了」，而是「没写、而使用者以为写了」。
// auth_token 未配置时网关会自动兜底（见 config.ResolveAuthToken），但兜底这件事本身
// 必须可见——否则使用者既不知道客户端为什么 401，也不知道自己刚得到一个令牌。
// 启动日志是唯一每次启动都能触达使用者的位置。
//
// 好消息与坏消息一并陈述：「没看到告警」和「看到没有告警」是两件事。
func logSecurityWarnings(cfg *config.Config, authTok string, generated bool) {
	switch {
	case cfg.Gateway.AllowUnauthenticated:
		log.Printf("[llmate-gate] SECURITY WARNING: authentication is DISABLED "+
			"(gateway.allow_unauthenticated=true). Anyone who can reach %s can call the upstream with "+
			"your configured API key, and can restore redacted text by request_id. "+
			"Only acceptable on an interface that is not reachable by others.", cfg.Gateway.Listen)
	case generated:
		log.Printf("[llmate-gate] gateway.auth_token is not configured — generated a random token and "+
			"stored it in %s (mode 0600).", cfg.Gateway.AuthTokenFile)
		log.Printf("[llmate-gate] NEW ACCESS TOKEN: %s", authTok)
		log.Printf("[llmate-gate] clients must send it as 'Authorization: Bearer <token>' or " +
			"'X-Api-Key: <token>'. The token is reused on the next restart, so it only has to be " +
			"configured once. Keep this line out of shared or uploaded logs.")
	case strings.TrimSpace(cfg.Gateway.AuthToken) != "":
		log.Printf("[llmate-gate] access token: from config (gateway.auth_token)")
	default:
		log.Printf("[llmate-gate] access token: loaded from %s", cfg.Gateway.AuthTokenFile)
	}

	if authTok != "" && strings.TrimSpace(cfg.Gateway.ControlAuthToken) == "" {
		log.Printf("[llmate-gate] control plane (/v1/privacy/*, can restore original text) shares the " +
			"data-plane token; set gateway.control_auth_token to separate the two capabilities.")
	}
	if cfg.Audit.Enabled && cfg.Audit.LogPII {
		log.Printf("[llmate-gate] SECURITY WARNING: audit.log_pii=true — detected original values are "+
			"written to %s in plaintext.", cfg.Audit.Path)
	}
}

// isLoopbackListen 判断监听地址是否只绑回环。
//
// 空 host（如 ":8400"）表示所有网卡，按非回环处理——那正是需要警惕的情形。
// 解析失败同样按非回环处理（fail-closed：宁可多打一条告警）。
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// loadRegistry 按配置装载登记表；未启用时返回 nil（Wrap 会原样返回内层检测器）。//
// 文件不存在视为空登记表：首次运行、或值全都由面板录入时就是这种状态。
// 其余失败（读不了 / YAML 坏了 / 值不合规）一律 fatal —— 登记表是用户对「这些值
// 一定会被脱敏」的承诺，带病启动等于悄悄毁约。
func loadRegistry(cfg *config.Config) *registry.Registry {
	if !cfg.Detection.Registry.Enabled {
		return nil
	}
	path := cfg.Detection.Registry.Path
	values := map[string][]string{}
	if vals, err := registry.LoadFile(path); err != nil {
		if !os.IsNotExist(err) {
			log.Fatalf("registry: %v", err)
		}
		log.Printf("[llmate-gate] registry file %s not found, starting empty", path)
	} else {
		values = vals
	}
	if err := registry.Validate(values); err != nil {
		log.Fatalf("%v", err)
	}
	reg := registry.New()
	reg.Set(values)
	log.Printf("[llmate-gate] registry enabled: %d entries in %d types (%s)",
		reg.Len(), len(reg.Get()), path)
	return reg
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