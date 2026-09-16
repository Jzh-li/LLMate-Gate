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
