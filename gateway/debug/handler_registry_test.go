package debug

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gateway/internal/config"
	"gateway/internal/registry"
	"gateway/pkg/types"

	"github.com/stretchr/testify/require"
)

// newRegistryHandler 构造带登记表的 Handler + 落盘路径。
func newRegistryHandler(t *testing.T, path string) (*Handler, *registry.Registry) {
	t.Helper()
	cfg := config.Default()
	cfg.Detection.Registry.Enabled = true
	cfg.Detection.Registry.Path = path

	reg := registry.New()
	h := NewHandler(Options{
		Config:   cfg,
		Hub:      NewHub(NewTrafficStore(8)),
		Registry: reg,
	})
	return h, reg
}

// doRegistry 发起一次 /_api/registry 请求。
func doRegistry(t *testing.T, h *Handler, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/_api/registry", nil)
	} else {
		r = httptest.NewRequest(method, "/_api/registry", strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	h.handleRegistry(w, r)
	return w
}

// TestRegistryAPI_GetEmpty 未登记时返回空表 + 类型清单 + 落盘路径。
func TestRegistryAPI_GetEmpty(t *testing.T) {
	h, _ := newRegistryHandler(t, filepath.Join(t.TempDir(), "registry.yaml"))
	w := doRegistry(t, h, http.MethodGet, "")
	require.Equal(t, http.StatusOK, w.Code)

	var got registryResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Empty(t, got.Registry)
	require.Equal(t, types.AllTypes(), got.Types, "面板下拉需要权威类型清单")
	require.Contains(t, got.Path, "registry.yaml")
}

// TestRegistryAPI_GetDoesNotEscapeHTML 值里的 & < 保持原样，面板才好读。
func TestRegistryAPI_GetDoesNotEscapeHTML(t *testing.T) {
	h, reg := newRegistryHandler(t, filepath.Join(t.TempDir(), "registry.yaml"))
	reg.Set(map[string][]string{types.EntityURL: {"https://x.cn/a?b=1&c=2"}})

	w := doRegistry(t, h, http.MethodGet, "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "b=1&c=2")
	// JSON 转义后的形式是反斜杠 + u0026（rune(92) 即反斜杠，避免转义地狱）
	require.NotContains(t, w.Body.String(), string(rune(92))+"u0026",
		"& 不该被转义，面板要照原样显示")
}

// TestRegistryAPI_PutDisabled 未启用登记表时明确 501 并给出开启方法。
func TestRegistryAPI_PutDisabled(t *testing.T) {
	h := NewHandler(Options{Config: config.Default(), Hub: NewHub(NewTrafficStore(8))})
	w := doRegistry(t, h, http.MethodPut, `{"registry":{"zh_person_name":["王小明"]}}`)
	require.Equal(t, http.StatusNotImplemented, w.Code)
	require.Contains(t, w.Body.String(), "detection.registry.enabled")
}

// TestRegistryAPI_PutHappyPath 合法内容：落盘 + 生效 + 通知订阅者。
func TestRegistryAPI_PutHappyPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "registry.yaml")
	h, reg := newRegistryHandler(t, path)

	changed := 0
	reg.OnChange(func() { changed++ })

	w := doRegistry(t, h, http.MethodPut,
		`{"registry":{"zh_person_name":["王小明","李四"],"email":["Me@Corp.cn"]}}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"ok":true}`, w.Body.String())

	// 生效：立刻能扫到
	require.Equal(t, 3, reg.Len())
	require.Len(t, reg.Scan("王小明 写信给 me@corp.cn"), 2)
	// 通知：缓存依赖它失效，否则刚登记的值对同一句话不生效
	require.Equal(t, 1, changed, "变更必须通知订阅者（刷检测缓存）")

	// 落盘：重启后仍在，且权限收紧
	got, err := registry.LoadFile(path)
	require.NoError(t, err)
	require.Equal(t, []string{"王小明", "李四"}, got[types.EntityPersonName])

	fi, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

// TestRegistryAPI_PutInvalidRejected 非法内容必须被拒，且既不改内存也不落盘。
func TestRegistryAPI_PutInvalidRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.yaml")
	h, reg := newRegistryHandler(t, path)
	reg.Set(map[string][]string{types.EntityPersonName: {"原有值"}})

	cases := []struct {
		name string
		body string
		want string
	}{
		{"单字符值", `{"registry":{"zh_person_name":["我"]}}`, "too short"},
		{"未知类型", `{"registry":{"zh_person":["王小明"]}}`, "unknown entity type"},
		{"跨类型重复", `{"registry":{"zh_person_name":["王小明"],"zh_address":["王小明"]}}`, "already registered"},
		{"首尾空白", `{"registry":{"zh_person_name":[" 王小明 "]}}`, "whitespace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doRegistry(t, h, http.MethodPut, tc.body)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), tc.want)
			require.Equal(t, 1, reg.Len(), "内存登记表必须保持原样")
			require.Equal(t, "原有值", reg.Get()[types.EntityPersonName][0])
			_, err := os.Stat(path)
			require.True(t, os.IsNotExist(err), "非法内容绝不能落盘")
		})
	}
}

// TestRegistryAPI_PutSaveFailureKeepsState 落盘失败时内存也不变：
// 否则用户会以为存住了，重启后值消失。
func TestRegistryAPI_PutSaveFailureKeepsState(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	// 路径穿过一个普通文件 → MkdirAll 必失败
	h, reg := newRegistryHandler(t, filepath.Join(blocker, "registry.yaml"))
	reg.Set(map[string][]string{types.EntityPersonName: {"原有值"}})

	w := doRegistry(t, h, http.MethodPut, `{"registry":{"zh_person_name":["王小明"]}}`)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Contains(t, w.Body.String(), "registry_save_failed")
	require.Equal(t, "原有值", reg.Get()[types.EntityPersonName][0], "落盘失败不该改内存")
}

// TestRegistryAPI_MethodNotAllowed 只接受 GET / PUT。
func TestRegistryAPI_MethodNotAllowed(t *testing.T) {
	h, _ := newRegistryHandler(t, filepath.Join(t.TempDir(), "registry.yaml"))
	w := doRegistry(t, h, http.MethodDelete, "")
	require.Equal(t, http.StatusMethodNotAllowed, w.Code)
	require.Contains(t, w.Header().Get("Allow"), "GET")
}
