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

// panelCookieName 调试面板的凭据 Cookie 名。
//
// 面板是给人用浏览器打开的，而浏览器不会替你带 Authorization 头；WebSocket 的浏览器
// API 也无法设置自定义请求头。让凭据能进浏览器的唯一顺手通道就是 Cookie：访问一次
// /_debug?token=<令牌> 换成 HttpOnly Cookie，之后页面里的 fetch 与 WebSocket 自动带上。
//
// SameSite=Strict 是这个面板的 CSRF 防线——面板不做 CSRF token，Strict 让跨站页面
// 发起的请求根本带不上这份凭据。
const panelCookieName = "lmgate_panel"

// Server 网关 HTTP 服务。
//
// 分两个面：数据面（/v1/* LLM 转发、/metrics）与控制面（/v1/privacy/*、调试面板数据端点，
// 二者都能读到明文 PII）。两面用各自的令牌，理由见 controlAuthMiddleware。
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
	// debugRoutes 调试面板是否已挂到控制面 mux 上（由 main 调 AdoptDebugRoutes 声明）。
	debugRoutes bool
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

// Mux 暴露数据面路由，供 main 挂载附加路由。
//
// 注意：调试面板不要挂这里（见 ControlMux）。数据面令牌只够「转发」，而面板能读到
// 请求明文与登记表，属于控制面能力。
func (s *Server) Mux() *http.ServeMux { return s.mux }

// ControlMux 暴露控制面路由，供 main 挂载调试面板等「可读到明文」的附加路由。
//
// 调用方挂载之后必须调 AdoptDebugRoutes() 声明，否则这些路径不会被路由到这里。
func (s *Server) ControlMux() *http.ServeMux { return s.ctrlMux }

// AdoptDebugRoutes 声明调试面板已挂到 ControlMux 上。
//
// 分成「挂载」与「声明」两步而不是一个方法，是因为路径集合属于 server 层的路由决策
// （谁用什么令牌），而挂载需要 debug 包的依赖。分开后 server 不必 import debug。
func (s *Server) AdoptDebugRoutes() { s.debugRoutes = true }

// Handler 返回完整 HTTP handler（含鉴权中间件）。
//
// 路由分发的顺序即安全边界，三条各自独立：
//
//	/healthz      匿名可访问 —— 存活探针
//	/v1/privacy/  控制面令牌 —— 可还原原文
//	/_api/ /ws/…  控制面令牌 —— 可读到请求明文（调试面板数据端点）
//	/_debug       匿名但仅回环 —— 面板静态壳，不含任何请求数据
//	/             数据面令牌 —— 只转发
//
// 用独立的 root mux 而不是把中间件套在整个 mux 外层：后者只能给所有路径同一个
// 令牌，而 healthz 与 /v1/privacy/* 恰好是两个相反的极端（一个必须匿名、一个
// 权限最高），塞不进同一条规则里。
func (s *Server) Handler() http.Handler {
	root := http.NewServeMux()
	root.HandleFunc("/healthz", s.handleHealthz)
	root.Handle("/v1/privacy/", s.controlAuthMiddleware(s.ctrlMux))
	if s.debugRoutes {
		// 静态壳（HTML/JS/CSS）保持可达且不鉴权：它不含任何请求数据，而且是「输入令牌」
		// 这个动作的载体——壳本身要令牌就没人能完成第一次认证。能读到明文的是数据端点。
		root.Handle("/_debug", s.panelBootstrap(s.ctrlMux))
		root.Handle("/_debug/", s.panelBootstrap(s.ctrlMux))
		// 数据端点：流量详情、Playground（提交文本取 PII）、登记表（明文 PII）、
		// 规则热加载。与 /v1/privacy/* 同级：都能把已脱敏的内容还原成明文。
		root.Handle("/_api/", s.panelAuthMiddleware(s.ctrlMux))
		root.Handle("/ws/events", s.panelAuthMiddleware(s.ctrlMux))
	}
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
	return s.tokenMiddleware(s.authTok, next, "data plane", false)
}

// controlAuthMiddleware 控制面鉴权：/v1/privacy/*。
//
// 独立一层的原因：控制面能按 request_id 把占位符还原成原文，数据面只能转发。
// 给 Claude Code hooks 的那份凭据只该用来「进脱敏、出还原」，不该同时具备
// 「还原任意历史请求原文」的能力——两者权限不在一个级别。
//
// 未配置 control_auth_token 时回退到数据面令牌，单令牌部署的行为与改动前一致。
func (s *Server) controlAuthMiddleware(next http.Handler) http.Handler {
	return s.tokenMiddleware(s.controlToken(), next, "control plane", false)
}

// panelAuthMiddleware 调试面板数据端点鉴权：在控制面令牌基础上额外接受面板 Cookie。
//
// 多一条 Cookie 通道不是放宽，而是把「浏览器怎么携带凭据」这件事解决掉：浏览器在
// fetch 与 WebSocket 上都不会带 Authorization 头，只认 Cookie。Cookie 由
// /_debug?token=<令牌> 一次性换取（panelBootstrap），值即控制面令牌本身，
// 因此并没有引入第二份凭据——只是同一份令牌多了一个载体。
func (s *Server) panelAuthMiddleware(next http.Handler) http.Handler {
	return s.tokenMiddleware(s.controlToken(), next, "control plane", true)
}

// controlToken 控制面实际生效的令牌；未配置 control_auth_token 时回退数据面令牌。
func (s *Server) controlToken() string {
	if s.ctrlTok != "" {
		return s.ctrlTok
	}
	return s.authTok
}

// panelBootstrap 处理 /_debug?token=<令牌>：校验令牌、下发 HttpOnly Cookie、
// 302 去掉查询串——把令牌从地址栏和浏览历史里赶出去。
//
// 不带 token 参数时原样交给下游（正常渲染面板壳）。壳保持匿名可达是刻意的：
// 没有它就没有地方输入令牌。
func (s *Server) panelBootstrap(next http.Handler) http.Handler {
	tok := s.controlToken()
	if tok == "" {
		// 显式免鉴权部署：面板保持改动前的行为（仅回环可达）。
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("token")
		if q == "" {
			next.ServeHTTP(w, r)
			return
		}
		if subtle.ConstantTimeCompare([]byte(q), []byte(tok)) != 1 {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "invalid panel token\n")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     panelCookieName,
			Value:    tok,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
		// 回到不带查询串的同一路径：令牌不留在地址栏 / 历史 / Referer 里。
		dest := r.URL.Path
		if dest == "" {
			dest = "/_debug"
		}
		http.Redirect(w, r, dest, http.StatusFound)
	})
}

// tokenMiddleware 统一鉴权：token 为空表示调用方已决定本次启动不鉴权，直接放行。
//
// allowPanelCookie=true 时额外接受面板 Cookie（见 panelAuthMiddleware）。
func (s *Server) tokenMiddleware(token string, next http.Handler, scope string, allowPanelCookie bool) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := authenticate(r, token)
		if !ok && allowPanelCookie {
			ok = authenticatePanelCookie(r, token)
		}
		if !ok {
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

// authenticatePanelCookie 校验面板 Cookie（仅调试面板数据端点使用）。
func authenticatePanelCookie(r *http.Request, token string) bool {
	c, err := r.Cookie(panelCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(token)) == 1
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
