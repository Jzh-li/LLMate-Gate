// Package server HTTP 服务层：路由 / 鉴权 / healthz / metrics（契约 §4）。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	gatewayerrors "gateway/internal/errors"
	"gateway/internal/config"
	"gateway/internal/metrics"
	"gateway/internal/proxy"
)

// Server 网关 HTTP 服务。
//
// 分两个面：数据面（/v1/* LLM 转发、/metrics）与控制面（/v1/privacy/*，可还原原文）。
// 两者用各自的令牌，理由见 controlAuthMiddleware。
type Server struct {
	proxy         *proxy.Proxy
	cfg           *config.Config
	m             *metrics.Collectors
	authTok       string
	ctrlTok       string
	healthFn      func(ctx context.Context) error
	mux           *http.ServeMux
	ctrlMux       *http.ServeMux
	metricHandler http.Handler
}

// Options 构造选项。
type Options struct {
	Proxy     *proxy.Proxy
	Config    *config.Config
	Metrics   *metrics.Collectors
	AuthToken string
	// ControlAuthToken 控制面令牌；为空时回退到 AuthToken。
	ControlAuthToken string
	Health           func(ctx context.Context) error
}

// New 构造服务并注册路由。
func New(o Options) *Server {
	s := &Server{
		proxy:    o.Proxy,
		cfg:      o.Config,
		m:        o.Metrics,
		authTok:  o.AuthToken,
		ctrlTok:  o.ControlAuthToken,
		healthFn: o.Health,
		mux:      http.NewServeMux(),
		ctrlMux:  http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// Mux 暴露数据面路由，供 main 挂载调试面板等附加路由。
func (s *Server) Mux() *http.ServeMux { return s.mux }

// Handler 返回完整 HTTP handler（含鉴权中间件）。
//
// 路由分发的顺序即安全边界，三条各自独立：
//
//	/healthz      匿名可访问 —— 存活探针
//	/v1/privacy/  控制面令牌 —— 可还原原文
//	/             数据面令牌 —— 只转发
//
// 用独立的 root mux 而不是把中间件套在整个 mux 外层：后者只能给所有路径同一个
// 令牌，而 healthz 与 /v1/privacy/* 恰好是两个相反的极端（一个必须匿名、一个
// 权限最高），塞不进同一条规则里。
func (s *Server) Handler() http.Handler {
	root := http.NewServeMux()
	root.HandleFunc("/healthz", s.handleHealthz)
	root.Handle("/v1/privacy/", s.controlAuthMiddleware(s.ctrlMux))
	root.Handle("/", s.authMiddleware(s.mux))
	return root
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/v1/chat/completions", wrap(s.handleLLM("chat.completions")))
	s.mux.HandleFunc("/v1/completions", wrap(s.handleLLM("completions")))
	s.mux.HandleFunc("/v1/embeddings", wrap(s.handleLLM("embeddings")))
	s.mux.HandleFunc("/v1/responses", wrap(s.handleLLM("responses")))
	s.mux.HandleFunc("/v1/messages", wrap(s.handleLLM("messages")))
	s.mux.HandleFunc("/v1/models", wrap(s.proxy.Passthrough))
	s.mux.Handle("/metrics", s.metricsHandler())
	// 常驻隐私 API（不受 --no-debug 门控）：供 Claude Code hooks / VS Code 扩展等
	// 外部集成点递归脱敏与还原任意 JSON / 文本。挂在控制面 mux 上——它比数据面多一项
	// 「按 request_id 还原原文」的能力，不该和数据面共用同一个凭据。
	s.ctrlMux.HandleFunc("/v1/privacy/redact", s.proxy.PrivacyRedact)
	s.ctrlMux.HandleFunc("/v1/privacy/restore", s.proxy.PrivacyRestore)
}

// handleLLM 包装 LLM 端点：先读 body 判定 stream，再把同一份 body 交给 proxy。
//
// 注意不要在这里把 body 重新包成 io.NopCloser 再让 proxy 读第二遍 —— 那会让每个
// 请求的 body 在内存里被完整拷贝两次。body 直接以切片形式传下去。
func (s *Server) handleLLM(endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stream, body := detectStream(r)
		s.proxy.HandleWithBody(w, r, endpoint, stream, body)
	}
}

// detectStream 读取请求体并判定是否流式（避免 proxy 重复读）。
func detectStream(r *http.Request) (bool, []byte) {
	if r.Body == nil {
		return false, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false, nil
	}
	_ = r.Body.Close()
	stream := false
	var probe struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(body, &probe) == nil {
		stream = probe.Stream
	}
	return stream && r.Method == http.MethodPost, body
}

// handleHealthz 存活探针，匿名可访问。
//
// 刻意不鉴权：探针（CI / 安装脚本 / 编辑器扩展）通常不持有令牌，而把「服务没起来」
// 和「没带令牌」混成同一个 401 只会让启动排查更难。代价是暴露 detector 的可用性，
// 不含任何请求内容或凭据，可以接受。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	healthy := true
	msg := "ok"
	if s.healthFn != nil {
		if err := s.healthFn(r.Context()); err != nil {
			healthy = false
			msg = "detector unavailable: " + err.Error()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if !healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"status": map[bool]string{true: "ok", false: "degraded"}[healthy], "detail": msg})
}

func (s *Server) metricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 由 main 注入 promhttp.Handler；此处兜底为简单文本。
		if s.metricHandler != nil {
			s.metricHandler.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("# metrics endpoint not configured\n"))
	})
}

// SetMetricHandler 注入 prometheus HTTP handler（main 中构造）。
func (s *Server) SetMetricHandler(h http.Handler) { s.metricHandler = h }

// authMiddleware 数据面鉴权：/v1/* 转发端点与 /metrics。
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return s.tokenMiddleware(s.authTok, next, "data plane")
}

// controlAuthMiddleware 控制面鉴权：/v1/privacy/*。
//
// 独立一层的原因：控制面能按 request_id 把占位符还原成原文，数据面只能转发。
// 给 Claude Code hooks 的那份凭据只该用来「进脱敏、出还原」，不该同时具备
// 「还原任意历史请求原文」的能力——两者权限不在一个级别。
//
// 未配置 control_auth_token 时回退到数据面令牌，单令牌部署的行为与改动前一致。
func (s *Server) controlAuthMiddleware(next http.Handler) http.Handler {
	tok := s.ctrlTok
	if tok == "" {
		tok = s.authTok
	}
	return s.tokenMiddleware(tok, next, "control plane")
}

// tokenMiddleware 统一鉴权：token 为空表示调用方已决定本次启动不鉴权，直接放行。
func (s *Server) tokenMiddleware(token string, next http.Handler, scope string) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authenticate(r, token) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]string{
					"code": string(gatewayerrors.CodeUnauthorized),
					// scope 写进消息：401 到底是「数据面凭据不对」还是「控制面凭据不对」
					// 是排查时最想知道的一件事，而这两条路径的凭据本就不同。
					"message": "missing or invalid credentials for " + scope,
				},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate 校验 Bearer / X-Api-Key。
//
// 用常量时间比较：逐字符短路的 == 会把「前多少个字符对了」泄漏在响应时间上，
// 令牌是唯一的边界，这里不该给攻击者任何反馈。
func authenticate(r *http.Request, token string) bool {
	if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
		return subtle.ConstantTimeCompare([]byte(v[7:]), []byte(token)) == 1
	}
	if v := r.Header.Get("X-Api-Key"); v != "" {
		return subtle.ConstantTimeCompare([]byte(v), []byte(token)) == 1
	}
	return false
}

// wrap 统一 recover，避免单个 handler panic 拖垮进程。
func wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]string{"code": string(gatewayerrors.CodeUpstreamError), "message": "internal panic"},
				})
			}
		}()
		h(w, r)
	}
}

// Start 启动 HTTP 服务（阻塞）；当 ctx 取消时优雅关闭并释放监听端口，
// 使进程随后退出——这是 main 信号处理的落点。若旧进程不退出，
// 依赖重启的脚本（如 e2e/ui_smoke.sh 的 --no-debug 重启）会永久挂死。
func (s *Server) Start(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:         addr,
		Handler:      s.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shCancel()
		_ = srv.Shutdown(shCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
