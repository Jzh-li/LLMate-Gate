package replacer

import (
	"strings"
	"testing"
)

func sseRestorer(pairs map[string]string) *SSERestorer {
	eps := make([]entryPair, 0, len(pairs))
	for k, v := range pairs {
		eps = append(eps, entryPair{sentinel: k, restoreTo: v})
	}
	return NewSSERestorer(NewStreamRestorer(eps))
}

// 核心场景：占位符被 SSE 帧拆到两个事件里，仍必须还原（E3 / 技术方案 §5）。
func TestSSERestorer_PlaceholderSplitAcrossEvents(t *testing.T) {
	r := sseRestorer(map[string]string{"<<email_1>>": "zhangsan@example.com"})

	ev1 := `data: {"choices":[{"delta":{"content":"我的邮箱 <<e"},"index":0}]}` + "\n\n"
	ev2 := `data: {"choices":[{"delta":{"content":"mail_1>>，谢谢"},"index":0}]}` + "\n\n"

	out1, _ := r.Write([]byte(ev1))
	if strings.Contains(string(out1), "<<e") {
		t.Fatalf("第一帧不应泄漏半截占位符: %q", string(out1))
	}
	if !strings.Contains(string(out1), `"content":"我的邮箱 "`) {
		t.Fatalf("第一帧应保留已确定部分: %q", string(out1))
	}

	out2, _ := r.Write([]byte(ev2))
	if !strings.Contains(string(out2), "zhangsan@example.com") {
		t.Fatalf("第二帧应还原出原值: %q", string(out2))
	}
	if strings.Contains(string(out2), "<<") || strings.Contains(string(out2), ">>") {
		t.Fatalf("第二帧不应残留占位符: %q", string(out2))
	}
}

// 非 data 行与 [DONE] 必须原样透传。
func TestSSERestorer_PreservesNonDataLines(t *testing.T) {
	r := sseRestorer(map[string]string{"<<zh_phone_1>>": "13800138000"})
	in := "event: content_block_delta\ndata: {\"delta\":{\"text\":\"<<zh_phone_1>>\"}}\nid: 42\n\ndata: [DONE]\n\n"
	out, _ := r.Write([]byte(in))
	got := string(out)
	if !strings.Contains(got, "event: content_block_delta") {
		t.Fatalf("event 行丢失: %q", got)
	}
	if !strings.Contains(got, "id: 42") {
		t.Fatalf("id 行丢失: %q", got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("[DONE] 被改动: %q", got)
	}
	if !strings.Contains(got, "13800138000") {
		t.Fatalf("未还原: %q", got)
	}
}

// 数字不能被 float64 转换破坏（UseNumber）。
func TestSSERestorer_PreservesNumbers(t *testing.T) {
	r := sseRestorer(map[string]string{"<<zh_phone_1>>": "13800138000"})
	in := `data: {"created":1788974109,"id":"chatcmpl-x","choices":[{"index":0,"delta":{"content":"<<zh_phone_1>>"}}]}` + "\n\n"
	out, _ := r.Write([]byte(in))
	got := string(out)
	if !strings.Contains(got, `"created":1788974109`) {
		t.Fatalf("数字被改写: %q", got)
	}
	if !strings.Contains(got, `"id":"chatcmpl-x"`) {
		t.Fatalf("id 被改写: %q", got)
	}
	if !strings.Contains(got, "13800138000") {
		t.Fatalf("未还原: %q", got)
	}
}

// 分块到达（一个事件被 TCP 切成两块）也要正确。
func TestSSERestorer_PartialEventAcrossWrites(t *testing.T) {
	r := sseRestorer(map[string]string{"<<zh_phone_1>>": "13800138000"})
	part1 := `data: {"choices":[{"delta":{"content":"电话 <<z`
	part2 := `h_phone_1>>"}}]}` + "\n\n"
	if out, _ := r.Write([]byte(part1)); len(out) != 0 {
		t.Fatalf("不完整帧不应提前下发: %q", string(out))
	}
	out, _ := r.Write([]byte(part2))
	if !strings.Contains(string(out), "13800138000") {
		t.Fatalf("未还原: %q", string(out))
	}
}

// 无关字段不能被动（键保留、值透传）。
func TestSSERestorer_NonTextKeysUntouched(t *testing.T) {
	r := sseRestorer(map[string]string{"<<zh_phone_1>>": "13800138000"})
	in := `data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":"stop"}]}` + "\n\n"
	out, _ := r.Write([]byte(in))
	got := string(out)
	if !strings.Contains(got, `"role":"assistant"`) || !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("非文本字段应原样保留: %q", got)
	}
}

// 流末尾仍卡在半截占位符 → 计入 orphan 且原样吐出（fail-safe，不丢数据）。
func TestSSERestorer_CloseFlushesRemainder(t *testing.T) {
	r := sseRestorer(map[string]string{"<<email_1>>": "a@b.com"})
	in := `data: {"choices":[{"delta":{"content":"邮箱 <<e"}}]}` + "\n\n"
	if _, err := r.Write([]byte(in)); err != nil {
		t.Fatalf("write: %v", err)
	}
	rest, _ := r.Close()
	if !strings.Contains(string(rest), "<<e") {
		t.Fatalf("Close 应吐出残留内容: %q", string(rest))
	}
}
