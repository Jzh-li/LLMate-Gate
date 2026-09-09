// Package server HTTP 服务层：路由 / 鉴权 / healthz / metrics（契约 §4）。
package server

import (
	"bytes"
	"context"
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
type Server struct {
	proxy        *proxy.Proxy
	cfg          *config.Config
	m            *metrics.Collectors
	authTok      string
	healthFn     func(ctx context.Context) error
	mux          *http.ServeMux
	metricHandler http.Handler
}

// Options 构造选项。
type Options struct {
	Proxy    *proxy.Proxy
	Config   *config.Config
	Metrics  *metrics.Collectors
	AuthToken string
	Health   func(ctx context.Context) error
}

// New 构造服务并注册路由。
func New(o Options) *Server {
	s := &Server{
		proxy:    o.Proxy,
		cfg:      o.Config,
		m:        o.Metrics,
		authTok:  o.AuthToken,
		healthFn: o.Health,
		mux:      http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// Mux 暴露内部路由，供 main 挂载调试面板等附加路由。
func (s *Server) Mux() *http.ServeMux { return s.mux }

// Handler 返回完整 HTTP handler（含鉴权中间件）。
func (s *Server) Handler() http.Handler {
	return s.authMiddleware(s.mux)
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/v1/chat/completions", wrap(s.handleLLM("chat.completions")))
	s.mux.HandleFunc("/v1/completions", wrap(s.handleLLM("completions")))
	s.mux.HandleFunc("/v1/embeddings", wrap(s.handleLLM("embeddings")))
	s.mux.HandleFunc("/v1/responses", wrap(s.handleLLM("responses")))
	s.mux.HandleFunc("/v1/messages", wrap(s.handleLLM("messages")))
	s.mux.HandleFunc("/v1/models", wrap(s.proxy.Passthrough))
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.Handle("/metrics", s.metricsHandler())
}

// handleLLM 包装 LLM 端点：先读 body 判定 stream，再交由 proxy 处理。
func (s *Server) handleLLM(endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stream, body := detectStream(r)
		r.Body = io.NopCloser(bytes.NewReader(body))
		s.proxy.Handle(w, r, endpoint, stream)
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

// authMiddleware 可选鉴权：配置了 auth_token 时要求 Bearer / X-Api-Key。
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	if s.authTok == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, _ := s.authenticate(r); !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]string{"code": string(gatewayerrors.CodeUnauthorized), "message": "missing or invalid credentials"},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authenticate(r *http.Request) (bool, string) {
	if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
		if v[7:] == s.authTok {
			return true, ""
		}
		return false, "bad bearer"
	}
	if v := r.Header.Get("X-Api-Key"); v == s.authTok {
		return true, ""
	}
	return false, "no credential"
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
