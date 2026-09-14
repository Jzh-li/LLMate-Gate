package debug

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"gateway/internal/pipeline"
)

// EventType 调试事件类型常量（契约 §10.2 / events.go）。
type EventType string

const (
	EventRequestReceived  EventType = "request.received"
	EventDetectionDone    EventType = "detection.done"
	EventReplacementDone  EventType = "replacement.done"
	EventUpstreamResponse EventType = "upstream.response"
	EventRestoreDone      EventType = "restore.done"
	EventRuleChanged      EventType = "rule.changed"
	EventRegistryChanged  EventType = "registry.changed"
)

// WSMessage 推送给前端的事件信封（契约 §10.2）。
type WSMessage struct {
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// Hub WebSocket 广播中心（UI设计 §1.2 hub.go）。
//
// 约束：
//   - Publish 非阻塞（缓冲 channel + drop 策略，绝不阻塞主流程）
//   - Hub 自身崩溃由 recover 捕获，不影响进程
//   - 多订阅者：每个 WS 连接独立 goroutine 写入
//   - 慢订阅者：单条消息写入超过 writeTimeout 则断开该连接（背压）
type Hub struct {
	store *TrafficRecordStore
	// 注册 / 注销
	mu      sync.RWMutex
	clients map[*subscriber]struct{}
	// 配置
	queueSize    int           // 每个订阅者 inbox 容量
	writeTimeout time.Duration // 单条消息写入超时
	// 统计
	droppedTotal atomic.Uint64 // 因 inbox 满丢弃的事件计数（被 slow subscriber 拖累）
	closed       atomic.Bool
}

// subscriber 单个 WS 订阅者的写入端。
type subscriber struct {
	conn *websocket.Conn
	in   chan *WSMessage
}

// NewHub 构造 Hub；store 用于推送时同步更新流量记录（可选，传 nil 跳过）。
func NewHub(store *TrafficRecordStore) *Hub {
	return &Hub{
		store:        store,
		clients:      make(map[*subscriber]struct{}),
		queueSize:    64,
		writeTimeout: 2 * time.Second,
	}
}

// Publish 非阻塞发布调试事件（满足 pipeline.EventPublisher 接口；nil 安全）。
//
// 行为：
//   - hub 关闭后静默丢弃
//   - 若 data 是 *pipeline.TrafficEvent 且 store != nil，则落库（按 RequestID 合并）
//   - 每条事件 fan-out 到所有订阅者 inbox；订阅者 inbox 满则该订阅者被记一次 dropped 并跳过本条
//   - panic 由 recover 捕获（绝不传播到调用方）
func (h *Hub) Publish(eventType string, data interface{}) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[debug.hub] recovered panic: %v", rec)
		}
	}()
	if h.closed.Load() {
		return
	}
	if data != nil {
		// 落库：仅 TrafficEvent 类型（与 pipeline.EventPublisher 约定一致）
		if h.store != nil {
			if te, ok := data.(*pipeline.TrafficEvent); ok {
				h.store.Push(te)
			}
		}
	}

	ev := &WSMessage{Type: eventType, Timestamp: time.Now()}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return // 不可序列化的事件静默丢弃，绝不阻塞主流程
		}
		ev.Data = raw
	}

	h.mu.RLock()
	subs := make([]*subscriber, 0, len(h.clients))
	for s := range h.clients {
		subs = append(subs, s)
	}
	h.mu.RUnlock()

	for _, s := range subs {
		select {
		case s.in <- ev:
		default:
			// 该订阅者太慢，跳过本条；不阻塞其它订阅者
			h.droppedTotal.Add(1)
		}
	}
}

// DroppedTotal 返回累计因订阅者背压丢弃的事件数（用于 /metrics）。
func (h *Hub) DroppedTotal() uint64 { return h.droppedTotal.Load() }

// SubscriberCount 当前活跃 WS 连接数。
func (h *Hub) SubscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Subscribe 注册一个新的 WS 连接；返回的 func() 用于注销。
//
// 连接生命周期：
//   - 启动 2 个 goroutine：writePump（发）/ readPump（保活 + 异常捕获）
//   - 任何 panic 都会被 recover 并安全断开
func (h *Hub) Subscribe(w http.ResponseWriter, r *http.Request) (func(), error) {
	if h.closed.Load() {
		return nil, errClosed
	}
	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		// 面板仅 127.0.0.1 绑定；不校验 Origin（同源）。
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}
	sub := &subscriber{
		conn: conn,
		in:   make(chan *WSMessage, h.queueSize),
	}
	h.mu.Lock()
	h.clients[sub] = struct{}{}
	h.mu.Unlock()

	unsubscribe := func() {
		h.mu.Lock()
		if _, ok := h.clients[sub]; ok {
			delete(h.clients, sub)
			close(sub.in)
		}
		h.mu.Unlock()
		_ = conn.Close()
	}

	go h.writePump(sub)
	go h.readPump(sub, unsubscribe)
	return unsubscribe, nil
}

// writePump 从 sub.in 读事件 → 编码 → 发到 conn；写超时断开慢订阅者。
func (h *Hub) writePump(sub *subscriber) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[debug.hub.writePump] recovered: %v", rec)
		}
	}()
	for ev := range sub.in {
		frame, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		_ = sub.conn.SetWriteDeadline(time.Now().Add(h.writeTimeout))
		if err := sub.conn.WriteMessage(websocket.TextMessage, frame); err != nil {
			return // 连接异常，readPump 会清理
		}
	}
}

// readPump 维持连接（处理 ping/pong + 异常断开）；不读取业务消息。
func (h *Hub) readPump(sub *subscriber, unsubscribe func()) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[debug.hub.readPump] recovered: %v", rec)
		}
		unsubscribe()
	}()
	sub.conn.SetReadLimit(4096)
	_ = sub.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	sub.conn.SetPongHandler(func(string) error {
		_ = sub.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	for {
		// 仅用来探测断开；忽略任何客户端消息。
		if _, _, err := sub.conn.ReadMessage(); err != nil {
			return
		}
	}
}

// Close 关闭 Hub：断开所有订阅者并停止新订阅。
func (h *Hub) Close() {
	if !h.closed.CompareAndSwap(false, true) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.clients {
		close(s.in)
		_ = s.conn.Close()
		delete(h.clients, s)
	}
}

var errClosed = closedErr{}

type closedErr struct{}

func (closedErr) Error() string { return "debug hub closed" }