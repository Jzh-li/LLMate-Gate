package audit

import (
	"fmt"
	"os"
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

// TestLoggerRotation 超过阈值时轮转：历史文件顺移，超出保留份数的被删除。
func TestLoggerRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	// 阈值设小（200B）：一条事件约 100+ 字节，几条就会触发一次轮转。
	l, err := NewLoggerRotating(path, true, false, 200, 2)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	for i := 0; i < 30; i++ {
		if err := l.Write(&Event{RequestID: fmt.Sprintf("req-%03d", i)}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	_ = l.Close()

	// 轮转发生了：至少存在一份历史文件
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("未发生轮转，缺少 %s: %v", path+".1", err)
	}
	// 保留份数上限：maxBackups=2 → 只该有 .1 / .2，不该有 .3
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("maxBackups=2 时不应保留 .3")
	}
	// 当前文件必须仍在正常写入（轮转后重开的那个）
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat current: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatalf("轮转后当前文件应为空并继续追加，实际为 0 字节说明轮转发生在写入之后")
	}
	// 权限：审计文件可能含 PII，必须是 0600
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("审计文件权限应为 0600，实际 %o", perm)
	}
}

// TestLoggerNoRotation 阈值 0 表示不轮转（保留旧行为，不产生任何 .1 文件）。
func TestLoggerNoRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	l, err := NewLoggerRotating(path, true, false, 0, 3)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	for i := 0; i < 50; i++ {
		if err := l.Write(&Event{RequestID: "x"}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	_ = l.Close()
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("maxSize=0 时不应轮转")
	}
}

// TestLoggerRotationSizeAccounting 重启后要接着已有文件的长度计算，不能从 0 重新数。
//
// 若 size 每次启动都归零，「文件已经很大但阈值永远算不到」会让轮转彻底失效。
func TestLoggerRotationSizeAccounting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	l1, err := NewLoggerRotating(path, true, false, 0, 3)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	for i := 0; i < 10; i++ {
		_ = l1.Write(&Event{RequestID: "x"})
	}
	_ = l1.Close()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// 用「比当前文件略大一点」的阈值重启：下一次写入就必须触发轮转
	l2, err := NewLoggerRotating(path, true, false, before.Size()+1, 2)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	if err := l2.Write(&Event{RequestID: "trigger"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = l2.Close()

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("重启后未按已有文件长度计算阈值，轮转失效: %v", err)
	}
}
