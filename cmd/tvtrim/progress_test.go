package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestRenderProgress(t *testing.T) {
	cases := []struct {
		name        string
		done, total int
		failed      int
		elapsed     time.Duration
		wantPercent string
		wantETA     bool
		wantFailed  bool
	}{
		{name: "刚开始", done: 0, total: 10, elapsed: 0, wantPercent: "0%"},
		{name: "进行中", done: 5, total: 10, elapsed: 5 * time.Second, wantPercent: "50%", wantETA: true},
		{name: "有失败", done: 5, total: 10, failed: 2, elapsed: 5 * time.Second, wantPercent: "50%", wantETA: true, wantFailed: true},
		{name: "已完成", done: 10, total: 10, elapsed: 10 * time.Second, wantPercent: "100%"},
	}
	for _, c := range cases {
		eta := etaOf(c.done, c.total, c.elapsed)
		got := renderProgress(c.done, c.total, c.failed, barWidth, c.elapsed, eta, "")
		if !strings.Contains(got, c.wantPercent) {
			t.Errorf("%s: 进度 %q 应含 %s", c.name, got, c.wantPercent)
		}
		if strings.Count(got, "#")+strings.Count(got, "-") != barWidth {
			t.Errorf("%s: 进度条宽度 = %d, 期望 %d", c.name,
				strings.Count(got, "#")+strings.Count(got, "-"), barWidth)
		}
		if etaShown := strings.Contains(got, "预计剩"); etaShown != c.wantETA {
			t.Errorf("%s: 是否显示预计剩余 = %v, 期望 %v (%q)", c.name, etaShown, c.wantETA, got)
		}
		if failedShown := strings.Contains(got, "失败"); failedShown != c.wantFailed {
			t.Errorf("%s: 是否显示失败数 = %v, 期望 %v (%q)", c.name, failedShown, c.wantFailed, got)
		}
	}
}

// TestRenderProgressZeroTotal 空任务集不该出现 NaN 或除零。
func TestRenderProgressZeroTotal(t *testing.T) {
	got := renderProgress(0, 0, 0, barWidth, 0, 0, "")
	if strings.Contains(got, "NaN") || strings.Contains(got, "%!") {
		t.Errorf("空任务集渲染异常: %q", got)
	}
	if !strings.Contains(got, "100%") {
		t.Errorf("没有任务应视为已完成: %q", got)
	}
}

func TestPercent(t *testing.T) {
	cases := []struct{ done, total, want int }{
		{0, 10, 0}, {1, 10, 10}, {3, 10, 30}, {10, 10, 100}, {11, 10, 100}, {0, 0, 100},
	}
	for _, c := range cases {
		if got := percent(c.done, c.total); got != c.want {
			t.Errorf("percent(%d,%d) = %d, 期望 %d", c.done, c.total, got, c.want)
		}
	}
}

func TestETAOf(t *testing.T) {
	// 10 个任务跑完 5 个用了 10s → 剩 5 个预计 10s。
	if got := etaOf(5, 10, 10*time.Second); got != 10*time.Second {
		t.Errorf("etaOf = %v, 期望 10s", got)
	}
	// 样本不足或已完成时为 0。
	for _, c := range []struct {
		done, total int
		elapsed     time.Duration
	}{{0, 10, time.Second}, {10, 10, time.Second}, {5, 10, 0}, {5, 0, time.Second}} {
		if got := etaOf(c.done, c.total, c.elapsed); got != 0 {
			t.Errorf("etaOf(%d,%d,%v) = %v, 期望 0", c.done, c.total, c.elapsed, got)
		}
	}
}

// TestProgressNonTTYWritesLines 非交互输出(bytes.Buffer)应拿到完整文本行。
func TestProgressNonTTYWritesLines(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, 10)
	for i := 0; i < 10; i++ {
		p.step("S01E01.mkv", true)
	}
	p.finish()

	out := buf.String()
	if !strings.Contains(out, "10/10") {
		t.Errorf("应输出最终进度, 实际 %q", out)
	}
	if strings.Contains(out, "\r") {
		t.Error("非交互输出不该含回车重绘符")
	}
	// 首次 + 完成 = 至少 2 行。
	if lines := strings.Count(strings.TrimSpace(out), "\n") + 1; lines < 2 {
		t.Errorf("输出行数 = %d, 期望至少 2", lines)
	}
}

// TestProgressCountsFailures 失败数应累计并在文本里体现。
func TestProgressCountsFailures(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, 3)
	p.step("a.mkv", true)
	p.step("b.mkv", false)
	p.step("c.mkv", true)
	p.finish()
	if done, failed := p.lastDone(); done != 3 || failed != 1 {
		t.Errorf("done=%d failed=%d, 期望 3/1", done, failed)
	}
	if out := buf.String(); !strings.Contains(out, "失败 1") {
		t.Errorf("输出应体现失败数, 实际 %q", out)
	}
}

func TestIsTTYFalseForBuffer(t *testing.T) {
	if isTTY(&bytes.Buffer{}) {
		t.Error("内存缓冲不是终端")
	}
}

func TestEllipsizeHead(t *testing.T) {
	s := "权力的游戏.S01E01.1080p.WEB-DL.mkv"
	got := ellipsizeHead(s, 12)
	if len([]rune(got)) != 12 {
		t.Errorf("截断后 rune 数 = %d, 期望 12 (%q)", len([]rune(got)), got)
	}
	if !strings.HasPrefix(got, "...") {
		t.Errorf("应以 ... 开头: %q", got)
	}
	// 短于上限时原样返回。
	if got := ellipsizeHead("短.mkv", 20); got != "短.mkv" {
		t.Errorf("短名不该被截断: %q", got)
	}
	// 中文不能被截成乱码。
	if got := ellipsizeHead("进击的巨人第三季第01集.mkv", 10); len([]rune(got)) != 10 {
		t.Errorf("中文截断后 rune 数 = %d (%q)", len([]rune(got)), got)
	}
}
