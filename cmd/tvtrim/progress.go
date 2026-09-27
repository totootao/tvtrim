package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// 自动识别要整片解码音轨。默认每部剧只抽样前几集(见 SamplePerShow),
// 但剧集库很大时仍有可观耗时,所以给阶段一一个可视进度:
// 交互终端单行重绘,输出重定向/写日志时降级为周期性一行,避免刷屏。

const (
	// barWidth 是进度条的字符宽度。
	barWidth = 24
	// nonTTYInterval 是非交互输出时的最小刷新间隔。
	nonTTYInterval = 5 * time.Second
	// nameWidth 是进度行里文件名的最大显示宽度。
	nameWidth = 28
)

// progress 跟踪一串任务的完成数,并按输出目标决定渲染方式。
type progress struct {
	w      io.Writer
	total  int
	tty    bool
	mu     sync.Mutex
	done   int
	failed int
	start  time.Time
	last   time.Time // 上一次真正输出的时间
	lastN  int       // 上一次输出的字符数(重绘时擦除残留用)
}

// newProgress 创建进度显示器。w 是终端时单行重绘,否则按间隔追加行。
func newProgress(w io.Writer, total int) *progress {
	return &progress{w: w, total: total, tty: isTTY(w), start: time.Now()}
}

// step 标记完成一个任务。ok 为 false 表示该项失败。
func (p *progress) step(name string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.done++
	if !ok {
		p.failed++
	}
	now := time.Now()
	elapsed := now.Sub(p.start)
	eta := etaOf(p.done, p.total, elapsed)

	if p.tty {
		line := "\r" + renderProgress(p.done, p.total, p.failed, barWidth, elapsed, eta, name)
		if pad := p.lastN - len(line); pad > 0 { // 变短了,用空格擦掉残留
			line += strings.Repeat(" ", pad)
		}
		p.lastN = len(line)
		fmt.Fprint(p.w, line)
		p.last = now
		return
	}
	// 非交互:首次立即输出,之后按间隔刷新,收尾时一定补一行。
	if p.last.IsZero() || now.Sub(p.last) >= nonTTYInterval || p.done >= p.total {
		fmt.Fprintln(p.w, renderProgress(p.done, p.total, p.failed, barWidth, elapsed, eta, ""))
		p.last = now
	}
}

// finish 收尾:TTY 下补一个换行,免得进度条盖住后续输出。
func (p *progress) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tty {
		fmt.Fprintln(p.w)
		return
	}
	// 非交互且还从未输出过(例如瞬间跑完):补一行最终结果。
	if p.total > 0 && p.last.IsZero() {
		fmt.Fprintln(p.w, renderProgress(
			p.done, p.total, p.failed, barWidth, time.Since(p.start), 0, ""))
	}
}

// lastDone 返回已完成数,供调用方收尾时显示。
func (p *progress) lastDone() (done, failed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done, p.failed
}

// renderProgress 渲染一行进度文本,纯函数便于测试。
// name 非空时追加在行尾(仅 TTY 重绘用);done>=total 时不显示剩余时间。
func renderProgress(done, total, failed, width int, elapsed, eta time.Duration, name string) string {
	var b strings.Builder
	if width > 0 && total > 0 {
		filled := done * width / total
		if filled > width {
			filled = width
		}
		b.WriteString("[")
		b.WriteString(strings.Repeat("#", filled))
		b.WriteString(strings.Repeat("-", width-filled))
		b.WriteString("] ")
	}
	b.WriteString(fmt.Sprintf("%d/%d %3d%%", done, total, percent(done, total)))
	b.WriteString("  已用 " + fmtDur(elapsed))
	if failed > 0 {
		b.WriteString(fmt.Sprintf("  失败 %d", failed))
	}
	if eta > 0 {
		b.WriteString("  预计剩 " + fmtDur(eta))
	}
	if name != "" {
		b.WriteString("  " + ellipsizeHead(name, nameWidth))
	}
	return b.String()
}

// percent 计算完成百分比;没有任务时视为已完成。
func percent(done, total int) int {
	if total <= 0 {
		return 100
	}
	if done <= 0 {
		return 0
	}
	if done >= total {
		return 100
	}
	return done * 100 / total
}

// etaOf 按已完成速度估算剩余时间,样本不足或已完成时返回 0。
func etaOf(done, total int, elapsed time.Duration) time.Duration {
	if done <= 0 || total <= 0 || done >= total || elapsed <= 0 {
		return 0
	}
	return (elapsed / time.Duration(done)) * time.Duration(total-done)
}

// isTTY 判断 w 是否为字符设备(终端)。重定向到文件或管道时为 false。
func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// ellipsizeHead 把超长文件名掐头保留尾部。按 rune 处理,避免截断多字节字符。
func ellipsizeHead(s string, max int) string {
	rs := []rune(s)
	if max <= 0 || len(rs) <= max {
		return s
	}
	if max <= 3 {
		return string(rs[:max])
	}
	return "..." + string(rs[len(rs)-(max-3):])
}
