package debug

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gateway/internal/config"

	"github.com/stretchr/testify/require"
)

// TestIsLoopbackAddr 回环判定：只认 127.0.0.0/8 与 ::1，解析失败一律 fail-closed。
func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want bool
	}{
		{"ipv4 回环带端口", "127.0.0.1:50000", true},
		{"ipv4 回环其他网段", "127.8.9.10:1234", true},
		{"ipv6 回环带端口", "[::1]:8080", true},
		{"ipv4 回环不带端口", "127.0.0.1", true},
		{"公网 IPv4", "203.0.113.7:50000", false},
		{"内网 IPv4", "10.0.0.1:1234", false},
		{"链路本地 IPv6", "[fe80::1]:1234", false},
		{"空串", "", false},
		{"乱码", "not-an-address", false},
		{"端口缺主机", ":8080", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, isLoopbackAddr(c.addr))
		})
	}
}

// TestMountLoopbackOnly 挂载后的调试路由：回环放行、非回环 403。
//
// 面板里有明文 PII 且不鉴权，这是唯一一道把它与 0.0.0.0 的 listen 隔开的闸。
func TestMountLoopbackOnly(t *testing.T) {
	h := NewHandler(Options{Config: config.Default()})
	mux := http.NewServeMux()
	h.Mount(mux)

	t.Run("回环放行", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/_api/registry", nil)
		r.RemoteAddr = "127.0.0.1:50000"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		require.Equal(t, http.StatusOK, w.Code)
	})
	t.Run("非回环拒绝", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/_api/registry", nil)
		r.RemoteAddr = "203.0.113.7:50000"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		require.Equal(t, http.StatusForbidden, w.Code)
		require.Contains(t, w.Body.String(), "loopback-only")
	})
	t.Run("WebSocket 同样受保护", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/ws/events", nil)
		r.RemoteAddr = "203.0.113.7:50000"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		require.Equal(t, http.StatusForbidden, w.Code)
	})
}
