// Package trim 实现"去头去尾"的核心计算与执行。
//
// 去头去尾的本质:把 [head, duration-tail] 这个时间窗口内的数据原样搬运到新文件。
// 因为 ffmpeg-trim 只做 stream copy,窗口边界会被对齐到关键帧/音频帧,
// 这点对"砍掉固定片头曲时长"这类场景完全够用。
package trim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/totootao/tvtrim/internal/ffmpeg"
)

// Options 描述一次去头去尾的裁剪任务。
type Options struct {
	// Head 是要砍掉的片头时长(从 0 开始算)。
	Head time.Duration
	// Tail 是要砍掉的片尾时长(从文件末尾往回算)。
	Tail time.Duration
	// Keep 是"总时长减去头尾后,再额外保留多少"。
	// 用于安全余量:比如片尾要留 5 秒尾巴时不至于把正片误删。
	// 该值会从输出窗口的末尾扣除(即会让输出更短),默认 0。
	Keep time.Duration
	// MinDuration 是输出时长下限。低于该值时拒绝裁剪,避免把片子剪没了。
	MinDuration time.Duration
	// Overwrite 决定目标文件已存在时的行为。
	Overwrite bool
	// Suffix 是输出文件名后缀,默认 "-trim"(即 a.mp4 -> a-trim.mp4)。
	// 设为 "inplace" 表示原地替换(先写临时文件再原子替换)。
	Suffix string
	// Output 显式指定输出文件路径(仅单文件模式有效)。非空时忽略 Suffix。
	Output string
	// DryRun 为 true 时只计算不执行。
	DryRun bool
	// Verbose 透传 ffmpeg 的完整输出。
	Verbose bool
}

// Plan 是一次裁剪的计划:计算出的时间窗口与输入输出路径。
type Plan struct {
	Input     string
	Output    string
	Duration  time.Duration
	Start     time.Duration // -ss: 砍掉片头后的起点
	End       time.Duration // -to: 砍掉片尾后的终点
	OutLength time.Duration
	Head      time.Duration
	Tail      time.Duration
	Probe     *ffmpeg.ProbeResult
	Skip      bool   // true 表示该文件被跳过(时长不足等)
	SkipWhy   string // 跳过原因
}

// WindowSeconds 返回传给 ffmpeg 的 (start, end) 秒数。
func (p *Plan) WindowSeconds() (float64, float64) {
	return p.Start.Seconds(), p.End.Seconds()
}

// DefaultMinDuration 是默认的输出时长下限(10 秒)。
const DefaultMinDuration = 10 * time.Second

// DefaultSuffix 是默认的输出文件名后缀。
const DefaultSuffix = "-trim"

// inPlaceSuffix 是"原地替换"的哨兵值。
const inPlaceSuffix = "inplace"

// BuildPlan 根据探测结果与选项计算裁剪计划。
//
// 计算规则:
//
//	start = head
//	end   = duration - tail
//	若 Keep > 0,则 end -= Keep(即结尾多砍一点,留出安全余量)
//
// 若 end-start < MinDuration(或 < 2 秒),标记为跳过。
func BuildPlan(p *ffmpeg.ProbeResult, opts Options) *Plan {
	plan := &Plan{
		Input:    p.Path,
		Duration: p.Duration,
		Head:     opts.Head,
		Tail:     opts.Tail,
		Probe:    p,
		Output:   OutputPath(p.Path, opts),
	}

	durSec := p.Duration.Seconds()
	start := opts.Head
	end := p.Duration - opts.Tail - opts.Keep

	// 钳制到合理范围,避免负数或超出。
	if start < 0 {
		start = 0
	}
	if end > p.Duration {
		end = p.Duration
	}
	if start > p.Duration {
		start = p.Duration
	}
	// end 允许为负(片尾时长超过总时长),但起点不能被压到 0 以下,
	// 否则 ffmpeg 会收到负的 -to。这里把 end 夹到 0,上层会因 OutLength<=0 而跳过。
	if end < 0 {
		end = 0
	}

	min := opts.MinDuration
	if min <= 0 {
		min = DefaultMinDuration
	}

	plan.Start = start
	plan.End = end
	plan.OutLength = end - start

	switch {
	case end <= start:
		plan.Skip = true
		plan.SkipWhy = fmt.Sprintf("头尾时长(%.1fs + %.1fs)超过了文件总时长(%.1fs)",
			opts.Head.Seconds(), opts.Tail.Seconds(), durSec)
	case plan.OutLength < min:
		plan.Skip = true
		plan.SkipWhy = fmt.Sprintf("裁剪后仅剩 %.1fs,低于下限 %.1fs",
			plan.OutLength.Seconds(), min.Seconds())
	case opts.Head == 0 && opts.Tail == 0 && opts.Keep == 0:
		plan.Skip = true
		plan.SkipWhy = "未指定任何头尾时长,无需处理"
	}
	return plan
}

// OutputPath 计算输出文件路径。
func OutputPath(input string, opts Options) string {
	if opts.Output != "" {
		return opts.Output
	}
	suffix := opts.Suffix
	if suffix == "" {
		suffix = DefaultSuffix
	}
	if suffix == inPlaceSuffix {
		return input
	}
	ext := filepath.Ext(input)
	base := strings.TrimSuffix(input, ext)
	return base + suffix + ext
}

// IsInPlace 判断该计划是否为原地替换。
func (p *Plan) IsInPlace() bool {
	return p.Input == p.Output
}

// Executor 执行裁剪计划。
type Executor struct {
	FFmpeg *ffmpeg.Runner
	Opts   Options
	// Log 用于输出进度,可为 nil。
	Log func(format string, args ...any)
}

// Run 执行单个裁剪计划,返回输出路径。
func (e *Executor) Run(ctx context.Context, plan *Plan) (string, error) {
	if plan.Skip {
		return "", nil
	}
	if err := os.MkdirAll(filepath.Dir(plan.Output), 0o755); err != nil {
		return "", err
	}

	// 目标已存在且不允许覆盖时直接报错,避免误伤。
	if !plan.IsInPlace() && !e.Opts.Overwrite {
		if _, err := os.Stat(plan.Output); err == nil {
			return "", fmt.Errorf("输出文件已存在: %s(加 --overwrite 覆盖)", plan.Output)
		}
	}

	// 原地替换时先写临时文件,成功后再原子改名,避免源文件被破坏。
	writeTo := plan.Output
	var tmpPath string
	if plan.IsInPlace() {
		f, err := os.CreateTemp(filepath.Dir(plan.Output), "."+filepath.Base(plan.Output)+".tmp*")
		if err != nil {
			return "", err
		}
		tmpPath = f.Name()
		f.Close()
		os.Remove(tmpPath) // ffmpeg 需要自己创建该文件
		writeTo = tmpPath
		defer func() {
			// 失败时清理临时文件
			if _, err := os.Stat(tmpPath); err == nil {
				os.Remove(tmpPath)
			}
		}()
	}

	args := e.buildArgs(plan, writeTo)
	cmd := exec.CommandContext(ctx, e.FFmpeg.Path, args...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout

	var stderr strings.Builder
	if e.Opts.Verbose {
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stderr = &stderr
	}

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		// 只保留最后的错误行,避免刷屏。
		if msg != "" {
			lines := strings.Split(msg, "\n")
			if len(lines) > 4 {
				lines = lines[len(lines)-4:]
			}
			msg = strings.Join(lines, "\n")
		}
		return "", fmt.Errorf("ffmpeg 执行失败: %w\n%s", err, msg)
	}

	// 空输出检测:ffmpeg 对"容器能探测但流无法搬运"的输入(如未启用解码路径的
	// webm 裸 VP8)可能静默产出几百字节的空文件且退出码为 0。校验兜底。
	if st, err := os.Stat(writeTo); err == nil && st.Size() < 1024 {
		os.Remove(writeTo)
		return "", fmt.Errorf("输出异常(仅 %d 字节,疑似空文件),已删除: 输入流的编解码组件不受支持", st.Size())
	}

	if plan.IsInPlace() {
		if err := os.Rename(tmpPath, plan.Output); err != nil {
			return "", fmt.Errorf("替换原文件失败: %w", err)
		}
		if err := os.Chmod(plan.Output, 0o644); err != nil && !errors.Is(err, os.ErrPermission) {
			return "", err
		}
	}
	return plan.Output, nil
}

// buildArgs 组装 ffmpeg 参数。
//
// 关键点(踩坑记录):
//   - `-ss` 必须放在 `-i` 之后(输出侧定位)。
//     ffmpeg-trim 是被裁剪过的构建,把 `-ss` 放在 `-i` 之前会被静默忽略,
//     结果是片头根本没被砍掉。实测:输入侧 `-ss 5 -i in -to 25` 产出 25s(片头没砍),
//     而输出侧 `-i in -ss 5 -to 25` 才正确产出 20s。
//   - 输出侧 `-ss` 会让 ffmpeg 定位到 `-ss` 之前最近的关键帧再开始搬运,
//     因此输出首帧一定是关键帧 —— 即使源片含 B 帧,也不会出现"开头黑屏/绿屏"。
//   - 用相对时长 `-t`(终点-起点)而非绝对 `-to`:裁剪长度与具体 muxer 无关,
//     避免 asf/wmv 等容器按关键帧对齐、多保留一个 GOP 导致时长偏长。
//   - `-avoid_negative_ts make_zero` + `-fflags +genpts`:把输出时间戳归零并重排 PTS。
//     否则输出流保留源片原始时间戳(如从 90s 起播),部分播放器会黑屏、时长错或无法拖动。
//   - `-c copy`:原样搬运,不重编码(ffmpeg-trim 本来也没有编码器)。
//   - `-map 0`:保留所有流(多音轨、字幕轨)。
func (e *Executor) buildArgs(plan *Plan, output string) []string {
	args := []string{
		"-hide_banner",
		"-nostdin",
		"-y",
	}
	if !e.Opts.Verbose {
		args = append(args, "-loglevel", "error")
	}

	args = append(args, "-i", plan.Input)

	// -ss 放在 -i 之后(输出侧定位)。顺序必须是 ss 在前。
	if plan.Start > 0 {
		args = append(args, "-ss", formatSeconds(plan.Start))
	}
	// 相对时长 -t:裁剪长度与 muxer 无关,避免 asf/wmv 多留 GOP。
	args = append(args, "-t", formatSeconds(plan.OutLength))
	args = append(args,
		"-map", "0",
		"-c", "copy",
		// 归零时间戳 + 重排 PTS:根治"从源片时间点起播"导致的黑屏/时长错/DTS 非单调。
		"-avoid_negative_ts", "make_zero",
		"-fflags", "+genpts",
	)

	// mp4 输出加 faststart:把 moov 移到文件头,支持边下边播/快速起播。
	// ffmpeg-trim 未启用 zlib 不影响该 flag;对其他容器不适用,仅 mp4/mov 加。
	if m := muxerFor(output); m == "mp4" {
		args = append(args, "-movflags", "+faststart")
	}

	args = append(args, "-f", muxerFor(output), output)
	return args
}

// formatSeconds 把时长格式化成 ffmpeg 接受的高精度秒数。
func formatSeconds(d time.Duration) string {
	return fmt.Sprintf("%.3f", d.Seconds())
}

// muxerFor 根据输出文件扩展名选择 muxer。
// 覆盖 ffmpeg-trim v2 内置且可输出的全部容器,显式指定比让 ffmpeg 猜更稳。
func muxerFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v", ".mov":
		return "mp4"
	case ".mkv":
		return "matroska"
	case ".webm":
		return "webm"
	case ".ts":
		return "mpegts"
	case ".avi":
		return "avi"
	case ".mp3":
		return "mp3"
	case ".asf", ".wmv":
		return "asf"
	default:
		return "matroska"
	}
}

// Result 是单个文件的裁剪结果。
type Result struct {
	Plan    *Plan
	Output  string
	Err     error
	Elapsed time.Duration
}
