package audit

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestLoggerRingRecent 验证内存环形缓冲：写入超过 ringCap 后截断，Recent 返回最新在前。
func TestLoggerRingRecent(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLogger(filepath.Join(dir, "audit.log"), true, false)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	defer func() { _ = l.Close() }()

	const n = 250
	for i := 0; i < n; i++ {
		if err := l.Write(&Event{RequestID: fmt.Sprintf("%d", i)}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// 超过 ringCap(200) 应被截断
	all := l.Recent(1000)
	if len(all) != 200 {
		t.Fatalf("ring cap: got %d want 200", len(all))
	}
	// 最新在前：首条应为最后写入的 249
	if all[0].RequestID != "249" {
		t.Fatalf("newest-first[0]: got %s want 249", all[0].RequestID)
	}
	// 末条应为 50（窗口 [50,249] 共 200 条，反转后末条=窗口最旧=50）
	if all[len(all)-1].RequestID != "50" {
		t.Fatalf("newest-first[last]: got %s want 50", all[len(all)-1].RequestID)
	}

	// Recent(50) 返回最近 50 条，最新在前
	last50 := l.Recent(50)
	if len(last50) != 50 {
		t.Fatalf("Recent(50): got %d want 50", len(last50))
	}
	if last50[0].RequestID != "249" {
		t.Fatalf("Recent(50)[0]: got %s want 249", last50[0].RequestID)
	}
	if last50[49].RequestID != "200" {
		t.Fatalf("Recent(50)[last]: got %s want 200", last50[49].RequestID)
	}
}

// TestLoggerDisabledRecentNil 验证 audit 关闭时 Recent 返回 nil。
func TestLoggerDisabledRecentNil(t *testing.T) {
	l, err := NewLogger("", false, false)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	if got := l.Recent(10); got != nil {
		t.Fatalf("disabled Recent should be nil, got %d events", len(got))
	}
}
