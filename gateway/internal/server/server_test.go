package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件只测「路由分发 + 鉴权边界」，不触到 proxy / metrics 的实现：
// 命中的是 /healthz 与不存在的路径，因此 Options 里的 Proxy 可以为 nil。
//
// 断言方式说明：用 404 而不是 200 证明「通过了鉴权」——请求走到路由表却没匹配到
// 任何 handler，说明它已经穿过了中间件。这样测试不需要构造完整的 proxy 依赖。

func do(s *Server, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestHandler_HealthzIsAnonymous 存活探针必须能匿名访问。
//
// 探针（CI / 安装脚本 / 编辑器扩展）通常不持有令牌；若强制鉴权，「服务没起来」
// 与「没带令牌」会混成同一个 401，反而更难排查。
func TestHandler_HealthzIsAnonymous(t *testing.T) {
	s := New(Options{AuthToken: "data-token", ControlAuthToken: "ctrl-token"})
	rec := do(s, http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusOK, rec.Code, "healthz 不应要求凭据")
	require.Contains(t, rec.Body.String(), `"status"`)
}

// TestHandler_DataPlaneRequiresToken 数据面端点无凭据一律 401。
func TestHandler_DataPlaneRequiresToken(t *testing.T) {
	s := New(Options{AuthToken: "data-token"})

	rec := do(s, http.MethodGet, "/v1/nonexistent", nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 错的凭据同样拒绝
	rec = do(s, http.MethodGet, "/v1/nonexistent", map[string]string{"Authorization": "Bearer wrong"})
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 正确凭据：穿过鉴权层，落到「路由不存在」
	rec = do(s, http.MethodGet, "/v1/nonexistent", map[string]string{"Authorization": "Bearer data-token"})
	require.Equal(t, http.StatusNotFound, rec.Code, "正确凭据应通过鉴权")

	// X-Api-Key 是另一种受支持的凭据形式
	rec = do(s, http.MethodGet, "/v1/nonexistent", map[string]string{"X-Api-Key": "data-token"})
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestHandler_ControlPlaneRequiresOwnToken 控制面与数据面令牌互不通用。
//
// 这是阶段 B2 的核心不变量：控制面能按 request_id 还原原文，数据面只能转发。
// 数据面令牌若也能打控制面，那么「给 hooks 的脱敏凭据」等于同时拿到了
// 「还原任意历史请求原文」的能力。
func TestHandler_ControlPlaneRequiresOwnToken(t *testing.T) {
	s := New(Options{AuthToken: "data-token", ControlAuthToken: "ctrl-token"})

	// 无凭据 → 401
	rec := do(s, http.MethodPost, "/v1/privacy/restore", nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 数据面令牌不能用于控制面 → 401
	rec = do(s, http.MethodPost, "/v1/privacy/restore", map[string]string{"Authorization": "Bearer data-token"})
	require.Equal(t, http.StatusUnauthorized, rec.Code,
		"数据面令牌不得访问控制面（否则等于把「还原原文」能力给了转发凭据）")

	// 控制面令牌可以用 → 穿过鉴权层（请求体不合法，故不是 401）
	rec = do(s, http.MethodPost, "/v1/privacy/restore", map[string]string{"Authorization": "Bearer ctrl-token"})
	require.NotEqual(t, http.StatusUnauthorized, rec.Code)
}

// TestHandler_ControlPlaneTokenRejectedOnDataPlane 反向：控制面令牌不得用于数据面。
func TestHandler_ControlPlaneTokenRejectedOnDataPlane(t *testing.T) {
	s := New(Options{AuthToken: "data-token", ControlAuthToken: "ctrl-token"})
	rec := do(s, http.MethodGet, "/v1/nonexistent", map[string]string{"Authorization": "Bearer ctrl-token"})
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestHandler_ControlFallsBackToDataToken 未单独配置控制面令牌时回退到数据面令牌。
//
// 单令牌部署（改动前的常态）行为必须与从前一致，否则升级即断。
func TestHandler_ControlFallsBackToDataToken(t *testing.T) {
	s := New(Options{AuthToken: "shared-token"})
	rec := do(s, http.MethodPost, "/v1/privacy/restore", map[string]string{"Authorization": "Bearer shared-token"})
	require.NotEqual(t, http.StatusUnauthorized, rec.Code)
}

// TestHandler_EmptyTokenAllowsAll 令牌为空 = 调用方已明确决定本次启动不鉴权。
//
// 注意分工：决定「令牌是否为空」的是 config.ResolveAuthToken（它保证这个空值
// 只可能来自显式的 allow_unauthenticated），server 只负责照做。
func TestHandler_EmptyTokenAllowsAll(t *testing.T) {
	s := New(Options{})
	rec := do(s, http.MethodGet, "/v1/nonexistent", nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "无令牌时应直接进入路由")

	rec = do(s, http.MethodPost, "/v1/privacy/restore", nil)
	require.NotEqual(t, http.StatusUnauthorized, rec.Code)
}

// TestHandler_MetricsRequiresToken /metrics 属于数据面，泄露内部运行状况。
func TestHandler_MetricsRequiresToken(t *testing.T) {
	s := New(Options{AuthToken: "data-token"})
	rec := do(s, http.MethodGet, "/metrics", nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	rec = do(s, http.MethodGet, "/metrics", map[string]string{"Authorization": "Bearer data-token"})
	require.NotEqual(t, http.StatusUnauthorized, rec.Code)
}

// ---------- 调试面板 ----------
//
// 面板的数据端点（/_api/*、/ws/events）能看到请求明文与登记表，与 /v1/privacy/* 属于
// 同一种能力。从前它们挂在数据面 mux 上，只需要「转发用」的令牌就能读到别人发过的
// 原文——这是本轮要收口的第二处。

// doWithCookie 与 do 相同，但携带面板 Cookie。
func doWithCookie(s *Server, method, target, value string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: panelCookieName, Value: value})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestHandler_DebugPanelDataRequiresControlToken 面板数据端点归控制面。
func TestHandler_DebugPanelDataRequiresControlToken(t *testing.T) {
	s := New(Options{AuthToken: "data-token", ControlAuthToken: "ctrl-token"})
	s.AdoptDebugRoutes()

	for _, path := range []string{"/_api/traffic", "/_api/registry", "/ws/events"} {
		rec := do(s, http.MethodGet, path, nil)
		require.Equal(t, http.StatusUnauthorized, rec.Code, "%s 无凭据应 401", path)

		// 数据面令牌不得读面板数据：这正是改动前的问题
		rec = do(s, http.MethodGet, path, map[string]string{"Authorization": "Bearer data-token"})
		require.Equal(t, http.StatusUnauthorized, rec.Code,
			"%s 不得接受数据面令牌（否则转发凭据即可读取他人请求明文）", path)

		rec = do(s, http.MethodGet, path, map[string]string{"Authorization": "Bearer ctrl-token"})
		require.NotEqual(t, http.StatusUnauthorized, rec.Code, "%s 控制面令牌应通过", path)
	}
}

// TestHandler_DebugPanelShellIsAnonymous 面板静态壳保持匿名可达。
//
// 理由是机制性的：壳里没有请求数据，而「输入令牌」这个动作必须发生在某个可达的页面上。
// 壳若也要凭据，第一次认证就无从完成。
func TestHandler_DebugPanelShellIsAnonymous(t *testing.T) {
	s := New(Options{AuthToken: "data-token", ControlAuthToken: "ctrl-token"})
	s.AdoptDebugRoutes()

	rec := do(s, http.MethodGet, "/_debug", nil)
	require.NotEqual(t, http.StatusUnauthorized, rec.Code, "面板壳不应要求凭据")

	rec = do(s, http.MethodGet, "/_debug/app.js", nil)
	require.NotEqual(t, http.StatusUnauthorized, rec.Code, "面板静态资源不应要求凭据")
}

// TestHandler_DebugPanelRoutesOnlyWhenAdopted 未声明挂载时面板路径不存在。
//
// --no-debug 的行为依赖这条：既没有路由，也不该留下鉴权痕迹（404 而非 401），
// 否则「面板关掉了吗」会变成一个需要猜的问题。
func TestHandler_DebugPanelRoutesOnlyWhenAdopted(t *testing.T) {
	s := New(Options{AuthToken: "data-token"})
	// 刻意不调 AdoptDebugRoutes

	rec := do(s, http.MethodGet, "/_api/traffic", map[string]string{"Authorization": "Bearer data-token"})
	require.Equal(t, http.StatusNotFound, rec.Code)

	// 未声明时 /_debug 落在数据面：带数据面令牌应为 404（路由不存在）
	rec = do(s, http.MethodGet, "/_debug", map[string]string{"Authorization": "Bearer data-token"})
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestHandler_PanelCookieBootstrap 令牌换 Cookie 的全流程。
func TestHandler_PanelCookieBootstrap(t *testing.T) {
	s := New(Options{AuthToken: "data-token", ControlAuthToken: "ctrl-token"})
	s.AdoptDebugRoutes()

	t.Run("错误的令牌不换取 Cookie", func(t *testing.T) {
		rec := do(s, http.MethodGet, "/_debug?token=wrong", nil)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		require.Empty(t, rec.Result().Cookies(), "校验失败不得下发 Cookie")
	})

	t.Run("数据面令牌不能换取面板 Cookie", func(t *testing.T) {
		rec := do(s, http.MethodGet, "/_debug?token=data-token", nil)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("正确令牌 → 302 + HttpOnly Cookie + 地址栏不留令牌", func(t *testing.T) {
		rec := do(s, http.MethodGet, "/_debug?token=ctrl-token", nil)
		require.Equal(t, http.StatusFound, rec.Code)
		require.Equal(t, "/_debug", rec.Header().Get("Location"),
			"重定向必须去掉查询串，否则令牌留在地址栏与浏览历史里")

		cookies := rec.Result().Cookies()
		require.Len(t, cookies, 1)
		c := cookies[0]
		require.Equal(t, panelCookieName, c.Name)
		require.Equal(t, "ctrl-token", c.Value)
		require.True(t, c.HttpOnly, "Cookie 必须 HttpOnly：前端读不到，XSS 也就偷不走")
		require.Equal(t, http.SameSiteStrictMode, c.SameSite,
			"SameSite=Strict 是面板的 CSRF 防线（面板不做 CSRF token）")
	})

	t.Run("Cookie 可用于面板数据端点", func(t *testing.T) {
		rec := doWithCookie(s, http.MethodGet, "/_api/traffic", "ctrl-token")
		require.NotEqual(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("Cookie 不适用于 /v1/privacy/*（最小权限）", func(t *testing.T) {
		// 面板 Cookie 与 /v1/privacy/* 用的是同一份令牌值，但通道只对面板开放：
		// 浏览器里的一次会话不应该同时是一条「任意 JSON 还原」的通道。
		rec := doWithCookie(s, http.MethodPost, "/v1/privacy/restore", "ctrl-token")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("伪 Cookie 被拒", func(t *testing.T) {
		rec := doWithCookie(s, http.MethodGet, "/_api/traffic", "data-token")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("显式免鉴权部署：面板回到仅回环可达", func(t *testing.T) {
		s2 := New(Options{})
		s2.AdoptDebugRoutes()
		require.NotEqual(t, http.StatusUnauthorized, do(s2, http.MethodGet, "/_api/traffic", nil).Code)
		// 不带 token 参数时原样交给下游，不做任何重定向
		rec := do(s2, http.MethodGet, "/_debug", nil)
		require.NotEqual(t, http.StatusFound, rec.Code)
	})
}
