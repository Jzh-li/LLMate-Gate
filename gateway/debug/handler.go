package debug

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"gateway/internal/audit"
	"gateway/internal/config"
	"gateway/internal/detector"
	"gateway/internal/registry"
	"gateway/internal/replacer"
	"gateway/pkg/types"
)

//go:embed assets/*
var assetsFS embed.FS

// Assets FS 返回内嵌的静态资源（测试可用）。
func Assets() embed.FS { return assetsFS }

// AuditSource 提供近期审计事件查询（契约 §9，桌面 UI 审计面板数据源）。
type AuditSource interface {
	Recent(n int) []audit.Event
}

// Handler 调试面板路由聚合（契约 §10 / UI设计 §0-4）。
type Handler struct {
	hub   *Hub
	store *TrafficRecordStore
	cfg   *config.Config
	det   detector.Client
	repl  replacer.Replacer
	reg   *registry.Registry
	// 规则热加载钩子（外部设置）：PUT /_api/rules 时调用
	ruleHook func(strategy string) error
	rulesMu  sync.Mutex
	// auditSrc 近期审计事件源（nil 时 /_api/audit 返回空）
	auditSrc AuditSource
}

// Options 构造 Handler 的依赖。
//
// 用结构体而不是长参数列表：这些依赖大多可空（nil 时对应端点降级），
// 摊成位置参数后调用点会变成一长串难以核对的 nil。
type Options struct {
	Config   *config.Config
	Detector detector.Client
	Replacer replacer.Replacer
	Hub      *Hub
	Store    *TrafficRecordStore
	// RuleHook 策略热加载钩子；nil 时 PUT /_api/rules 返回 501。
	RuleHook func(strategy string) error
	// AuditSource 近期审计事件源；nil 时 /_api/audit 返回空。
	AuditSource AuditSource
	// Registry 登记表；nil（未启用）时 /_api/registry 返回 501。
	// 落盘路径取自 Config.Detection.Registry.Path。
	Registry *registry.Registry
}

// NewHandler 构造 debug Handler。
func NewHandler(o Options) *Handler {
	return &Handler{
		hub:      o.Hub,
		store:    o.Store,
		cfg:      o.Config,
		det:      o.Detector,
		repl:     o.Replacer,
		reg:      o.Registry,
		ruleHook: o.RuleHook,
		auditSrc: o.AuditSource,
	}
}

// Mount 把 debug 路由挂到 mux（受 --no-debug 门控由调用方决定）。
//
// 路由：
//
//	GET    /_debug         → HTML（embed）
//	GET    /_debug/...     → 静态资源（JS/CSS）
//	WS     /ws/events      → 流量事件推送
//	GET    /_api/traffic   → 拉取环形缓冲
//	DELETE /_api/traffic   → 清空
//	POST   /_api/detect    → Playground: 仅检测
//	POST   /_api/replace   → Playground: 检测 + 替换
//	GET    /_api/rules     → 当前规则
//	PUT    /_api/rules     → 更新策略（热加载）
//	GET    /_api/dictionary → 仿真词典
//	PUT    /_api/dictionary → 整体替换仿真词典（热加载）
//	GET    /_api/registry  → 登记表（明文 PII，仅回环可访问）
//	PUT    /_api/registry  → 整体替换登记表并落盘（热加载 + 刷检测缓存）
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/_debug", loopbackOnly(h.serveIndex))
	mux.HandleFunc("/_debug/", loopbackOnly(h.serveAsset))
	mux.HandleFunc("/ws/events", loopbackOnly(h.serveWS))
	mux.HandleFunc("/_api/traffic", loopbackOnly(h.handleTraffic))
	mux.HandleFunc("/_api/detect", loopbackOnly(h.handleDetect))
	mux.HandleFunc("/_api/replace", loopbackOnly(h.handleReplace))
	mux.HandleFunc("/_api/rules", loopbackOnly(h.handleRules))
	mux.HandleFunc("/_api/dictionary", loopbackOnly(h.handleDictionary))
	mux.HandleFunc("/_api/registry", loopbackOnly(h.handleRegistry))
	mux.HandleFunc("/_api/audit", loopbackOnly(h.handleAudit))
}

// loopbackOnly 只放行来自回环地址的请求。
//
// 调试面板能看到明文 PII（Playground 原文、流量详情、登记表），且默认不鉴权，
// 所以它不能随 listen 绑定到 0.0.0.0 就对外可达。这里按 TCP 对端地址（RemoteAddr）
// 判定，不读 X-Forwarded-For——那个头是客户端可控的，读了等于把守卫交出去。
func loopbackOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackAddr(r.RemoteAddr) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "debug panel is loopback-only; use --no-debug for remote deployments",
			})
			return
		}
		next(w, r)
	}
}

// isLoopbackAddr 判断 RemoteAddr 的对端主机是否为回环地址；解析失败一律视为非回环
// （fail-closed：宁可挡住，不可放行一个 PII 面板）。
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // 无端口形式（如裸 IP）
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// ---------- handlers ----------

// serveIndex 返回面板 HTML（embed）。
func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/_debug" {
		http.NotFound(w, r)
		return
	}
	data, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "panel asset missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 面板不缓存，便于开发期热更新（UI设计 §1）。
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// serveAsset 返回内嵌静态文件（JS/CSS）。
func (h *Handler) serveAsset(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[len("/_debug/"):]
	if name == "" {
		http.NotFound(w, r)
		return
	}
	// 简单安全：禁止路径穿越（embed FS 本身已禁止 ..）
	if strings.Contains(name, "..") || strings.ContainsAny(name, "\\") {
		http.NotFound(w, r)
		return
	}
	data, err := assetsFS.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctype := "text/plain; charset=utf-8"
	switch {
	case len(name) >= 3 && name[len(name)-3:] == ".js":
		ctype = "application/javascript; charset=utf-8"
	case len(name) >= 4 && name[len(name)-4:] == ".css":
		ctype = "text/css; charset=utf-8"
	case len(name) >= 5 && name[len(name)-5:] == ".html":
		ctype = "text/html; charset=utf-8"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// serveWS 把 HTTP 升级为 WebSocket。
func (h *Handler) serveWS(w http.ResponseWriter, r *http.Request) {
	if h.hub == nil {
		http.Error(w, "hub not configured", http.StatusServiceUnavailable)
		return
	}
	unsub, err := h.hub.Subscribe(w, r)
	if err != nil {
		// Upgrade 已写入响应，只能记日志
		log.Printf("[debug] ws upgrade failed: %v", err)
		return
	}
	_ = unsub // readPump 负责调用
}

// handleTraffic GET/DELETE 流量缓冲。
func (h *Handler) handleTraffic(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		// 注意：占位符 << / >> 在 JSON 序列化时默认转义为 \u003c/\u003e。
		// 用 json.Encoder + SetEscapeHTML(false) 序列化 records，让占位符字面保留。
		var recBuf bytes.Buffer
		recEnc := json.NewEncoder(&recBuf)
		recEnc.SetEscapeHTML(false)
		if err := recEnc.Encode(h.store.Snapshot()); err != nil {
			writeErr(w, http.StatusInternalServerError, "marshal_failed", err.Error())
			return
		}
		// recEnc.Encode 末尾会加 \n，去掉它
		recJSON := bytes.TrimRight(recBuf.Bytes(), "\n")
		body := fmt.Sprintf(`{"records":%s,"size":%d,"cap":%d}`, recJSON, h.store.Len(), h.store.Cap())
		_, _ = w.Write([]byte(body))
	case http.MethodDelete:
		h.store.Clear()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		w.Header().Set("Allow", "GET, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// detectReq POST /_api/detect 入参（契约 §10.3）。
type detectReq struct {
	Text     string   `json:"text"`
	Entities []string `json:"entities,omitempty"`
}

// detectResp POST /_api/detect 出参。
type detectResp struct {
	Entities  []entityResp `json:"entities"`
	LatencyMs int64        `json:"latency_ms"`
	Error     string       `json:"error,omitempty"`
}

type entityResp struct {
	Type       string  `json:"type"`
	Value      string  `json:"value"`
	Start      int     `json:"start"`
	End        int     `json:"end"`
	Confidence float64 `json:"confidence"`
}

// handleDetect Playground: 纯本地检测（不出站，UI设计 §3.3）。
func (h *Handler) handleDetect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req detectReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Text == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "text is empty")
		return
	}
	// 限流：单次文本不超过 64KB（Playground 是手动测试，不是生产流量）
	if len(req.Text) > 64*1024 {
		writeErr(w, http.StatusBadRequest, "invalid_request", "text too large (max 64KB)")
		return
	}
	// 直接用主 detector；detector.Client 接口未暴露运行时 entity filter，
	// 因此 Playground 的 entities 字段在本版本仅作为占位保留（文档说明）。
	_ = req.Entities

	start := time.Now()
	resp, err := h.det.Detect(r.Context(), &types.DetectRequest{Text: req.Text})
	latency := time.Since(start).Milliseconds()

	out := detectResp{LatencyMs: latency}
	if err != nil {
		out.Error = err.Error()
	} else {
		for _, e := range resp.Entities {
			out.Entities = append(out.Entities, entityResp{
				Type:       e.Type,
				Value:      e.Value,
				Start:      e.Start,
				End:        e.End,
				Confidence: float64(e.Score),
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// replaceReq POST /_api/replace 入参（契约 §10.3）。
type replaceReq struct {
	Text     string   `json:"text"`
	Strategy string   `json:"strategy,omitempty"` // placeholder | simulate
	Entities []string `json:"entities,omitempty"`
}

// replaceResp POST /_api/replace 出参。
type replaceResp struct {
	Replaced string         `json:"replaced"`
	Mapping  []MappingEntry `json:"mapping"`
	Strategy string         `json:"strategy"`
}

// handleReplace Playground: 纯本地检测 + 替换（不出站，UI设计 §3.3）。
//
// 不写 vault：Playground 数据不落盘（UI设计 §3.3 安全约束）。
func (h *Handler) handleReplace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req replaceReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Text == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "text is empty")
		return
	}
	if len(req.Text) > 64*1024 {
		writeErr(w, http.StatusBadRequest, "invalid_request", "text too large (max 64KB)")
		return
	}
	if req.Strategy != "" && req.Strategy != "placeholder" && req.Strategy != "simulate" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "strategy must be placeholder|simulate")
		return
	}
	_ = req.Entities // 同 handleDetect

	resp, err := h.det.Detect(r.Context(), &types.DetectRequest{Text: req.Text})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "detector_failed", err.Error())
		return
	}

	rr, err := h.repl.Replace(r.Context(), &replacer.ReplaceRequest{
		Text:           req.Text,
		ConversationID: "playground",
		Entities:       resp.Entities,
		Strategy:       req.Strategy,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "replace_failed", err.Error())
		return
	}

	out := replaceResp{
		Replaced: rr.Text,
		Mapping:  make([]MappingEntry, 0, len(rr.Entries)),
		Strategy: h.repl.Strategy(),
	}
	for _, e := range rr.Entries {
		out.Mapping = append(out.Mapping, MappingEntry{
			Placeholder: e.Sentinel(),
			Type:        e.EntityType,
			Value:       string(e.Original),
		})
	}
	// 关键：与 proxy.go 一致，关闭 HTML 转义，否则占位符的 << / >> 会被编码为 \u003c/\u003e。
	w.Header().Set("Content-Type", "application/json")
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
	_, _ = w.Write(buf.Bytes())
}

// rulesResp GET /_api/rules 出参。
type rulesResp struct {
	Strategy     string   `json:"strategy"`
	Irreversible []string `json:"irreversible"`
}

// rulesReq PUT /_api/rules 入参（只支持改 strategy，irreversible 需重启）。
type rulesReq struct {
	Strategy string `json:"strategy"`
}

// handleRules GET 读取当前策略；PUT 热更新 strategy。
func (h *Handler) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		strategy := "placeholder"
		if h.repl != nil {
			strategy = h.repl.Strategy()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rulesResp{
			Strategy:     strategy,
			Irreversible: h.cfg.Replacement.Irreversible,
		})
	case http.MethodPut:
		var req rulesReq
		if err := decodeJSON(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		switch req.Strategy {
		case "placeholder", "simulate", "bypass":
		default:
			writeErr(w, http.StatusBadRequest, "invalid_request", "strategy must be placeholder|simulate|bypass")
			return
		}
		h.rulesMu.Lock()
		hook := h.ruleHook
		h.rulesMu.Unlock()
		if hook == nil {
			writeErr(w, http.StatusNotImplemented, "not_implemented", "rule hot-reload not wired")
			return
		}
		if err := hook(req.Strategy); err != nil {
			writeErr(w, http.StatusInternalServerError, "rule_apply_failed", err.Error())
			return
		}
		// 广播 rule.changed（UI设计 §3 / §4.2）
		h.hub.Publish(string(EventRuleChanged), map[string]string{"strategy": req.Strategy})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// dictResp GET /_api/dictionary 出参。
type dictResp struct {
	Dictionary map[string]map[string]string `json:"dictionary"`
	// Types 可仿真的实体类型，供面板下拉渲染（顺序固定）。
	Types []string `json:"types"`
}

// dictReq PUT /_api/dictionary 入参。
type dictReq struct {
	Dictionary map[string]map[string]string `json:"dictionary"`
}

// handleDictionary GET 读取仿真词典；PUT 整体替换（热加载，无需重启）。
//
// 整体替换而非增量合并：面板持有完整视图，「删掉一条」只有整体写回才能表达。
// 校验复用 config.ValidateSimulateDictionary，与配置加载期同一套规则——
// 面板绕不过「同类型仿真值必须唯一」这条，否则还原表会静默冲突。
func (h *Handler) handleDictionary(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		dict := map[string]map[string]string{}
		if h.repl != nil {
			if d := h.repl.Dictionary(); d != nil {
				dict = d
			}
		}
		w.Header().Set("Content-Type", "application/json")
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(dictResp{Dictionary: dict, Types: config.SimulatableTypes()})
		_, _ = w.Write(buf.Bytes())
	case http.MethodPut:
		var req dictReq
		if err := decodeJSON(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if err := config.ValidateSimulateDictionary(req.Dictionary); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_dictionary", err.Error())
			return
		}
		if h.repl == nil {
			writeErr(w, http.StatusNotImplemented, "not_implemented", "replacer not wired")
			return
		}
		h.repl.SetDictionary(req.Dictionary)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// registryResp GET /_api/registry 出参。
type registryResp struct {
	Registry map[string][]string `json:"registry"`
	// Types 可登记的全部实体类型（契约 §6.1 权威表），供面板下拉渲染。
	Types []string `json:"types"`
	// Enabled 登记表是否已启用（未启用时面板提示去改配置，PUT 也是 501）。
	Enabled bool `json:"enabled"`
	// Path 登记表落盘路径，面板展示用——用户得知道自己的 PII 写到哪个文件了。
	Path string `json:"path"`
}

// registryReq PUT /_api/registry 入参。
type registryReq struct {
	Registry map[string][]string `json:"registry"`
}

// handleRegistry GET 读取登记表；PUT 整体替换（校验 → 落盘 → 生效）。
//
// 整体替换而非增量合并，理由同仿真词典：面板持有完整视图，「删掉一条」只有
// 整体写回才能表达。
//
// 顺序是「校验 → 落盘 → 生效」，每一步失败都就此打住：写坏文件再生效会让
// 下次启动加载失败、整张登记表全丢；生效了但没落盘则会让用户以为重启后还在。
// 生效那一步会触发 OnChange → 刷检测缓存，所以刚登记的值对同一句话立刻就管用。
func (h *Handler) handleRegistry(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		values := map[string][]string{}
		if h.reg != nil {
			if v := h.reg.Get(); v != nil {
				values = v
			}
		}
		w.Header().Set("Content-Type", "application/json")
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		// 值里可能有 < > &（URL 查询串、邮件签名），别转义成 < 让人看不懂
		enc.SetEscapeHTML(false)
		_ = enc.Encode(registryResp{
			Registry: values,
			Types:    types.AllTypes(),
			Enabled:  h.reg != nil,
			Path:     h.cfg.Detection.Registry.Path,
		})
		_, _ = w.Write(buf.Bytes())
	case http.MethodPut:
		var req registryReq
		if err := decodeJSON(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if h.reg == nil {
			writeErr(w, http.StatusNotImplemented, "not_implemented",
				"registry is disabled; set detection.registry.enabled=true (with detection.registry.path) and restart")
			return
		}
		if err := registry.Validate(req.Registry); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_registry", err.Error())
			return
		}
		path := h.cfg.Detection.Registry.Path
		if err := registry.SaveFile(path, req.Registry); err != nil {
			writeErr(w, http.StatusInternalServerError, "registry_save_failed", err.Error())
			return
		}
		h.reg.Set(req.Registry)
		h.hub.Publish(string(EventRegistryChanged), map[string]int{"entries": h.reg.Len()})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAudit GET /_api/audit 返回近期审计事件（最新在前）。
//
// 查询参数：?limit=N（默认 50，最大 500）。审计关闭（auditSrc=nil 或 disabled）时返回空数组。
// 数据源为 audit.Logger 的内存环形缓冲（契约 §9，桌面 UI 审计面板据此渲染）。
func (h *Handler) handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 50
	if s := r.URL.Query().Get("limit"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			limit = v
		}
	}
	events := []audit.Event{}
	if h.auditSrc != nil {
		if got := h.auditSrc.Recent(limit); got != nil {
			events = got
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"events": events,
		"count":  len(events),
	})
}

// ---------- helpers ----------

// decodeJSON 读 body 并解析；限制最大 1MB。
func decodeJSON(r *http.Request, v interface{}) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = r.Body.Close()
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// writeErr 写统一 JSON 错误（与 server.writeError 风格一致）。
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{"code": code, "message": msg},
	})
}
