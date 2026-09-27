// Package ffmpeg 负责定位、缓存并驱动 ffmpeg-trim 二进制。
//
// ffmpeg-trim 是一个刻意裁剪过的 FFmpeg(minimal trim build):
//   - 只有 mp3/avi/mp4/mkv/mpegts 的 demuxer+muxer
//   - 只有 aac / aac_latm 两个音频解码器,没有任何视频解码器
//   - 没有 ffprobe、没有网络协议、没有编码器
//
// 因此本包用 `ffmpeg -i <file>` 的 stderr 输出来代替 ffprobe 做媒体探测,
// 所有裁剪都走 stream copy(-c copy),绝不重编码。
package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrUnsupported 表示输入文件的容器格式不在 ffmpeg-trim 的支持范围内。
var ErrUnsupported = errors.New("不支持的容器格式")

// 常见 TV 剧集容器。顺序即目录扫描时的匹配优先级。
var SupportedExts = []string{".mp4", ".mkv", ".ts", ".avi", ".mp3", ".webm", ".asf", ".wmv"}

// Stream 描述输入文件中的一条流。
type Stream struct {
	Index  int
	Kind   string // "video" | "audio" | "subtitle" | "data"
	Codec  string // 例如 "h264" / "hevc" / "aac" / "mp3"
	Detail string // 原始描述行,用于 --verbose 展示
}

// ProbeResult 是一次媒体探测的结果。
type ProbeResult struct {
	Path        string
	Container   string // 探测出的容器名,例如 "mov,mp4,m4a,3gp,3g2,mj2"
	Duration    time.Duration
	Start       time.Duration // 容器起始时间戳
	Bitrate     int           // kb/s
	Streams     []Stream
	HasVideo    bool
	HasAudio    bool
	RawDuration string
}

// DurationSeconds 返回时长的秒数(浮点)。
func (p *ProbeResult) DurationSeconds() float64 {
	return p.Duration.Seconds()
}

// Runner 封装了对 ffmpeg 二进制的调用。
type Runner struct {
	// Path 是 ffmpeg 可执行文件的绝对路径。
	Path string
	// Verbose 为 true 时把 ffmpeg 的原始 stderr 透传到标准错误。
	Verbose bool
}

// reDuration 匹配 `  Duration: 00:01:23.45, start: 0.000000, bitrate: 1234 kb/s`
// 注意 start 可能是负数(部分 mkv 的 start 为 -0.023),必须允许前导负号。
var reDuration = regexp.MustCompile(`Duration:\s*(\d+):(\d{2}):(\d{2}(?:\.\d+)?)\s*,\s*start:\s*(-?[\d.]+)\s*,\s*bitrate:\s*(\d+)\s*kb/s`)

// reStream 匹配 `  Stream #0:1[0x2](und): Audio: aac (mp4a / 0x6134706D), 44100 Hz, ...`
var reStream = regexp.MustCompile(`Stream #(\d+):(\d+)(?:\[[^\]]*\])?(?:\([^)]*\))?:\s*(Video|Audio|Subtitle|Data|Attachment):\s*([A-Za-z0-9_]+)`)

// reInvalidData 匹配容器不被识别时的报错。
var reInvalidData = regexp.MustCompile(`Invalid data found when processing input`)

// Probe 通过解析 `ffmpeg -i <path>` 的 stderr 获取媒体信息。
//
// ffmpeg-trim 没有 ffprobe,但 `-i` 单独使用时会把 Input 段打印到 stderr
// 并以退出码 1 结束(因为"没有指定输出文件"),这正好可以用来探测。
func (r *Runner) Probe(ctx context.Context, path string) (*ProbeResult, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("读取文件失败 %s: %w", path, err)
	} else if st.IsDir() {
		return nil, fmt.Errorf("%s 是目录,不是文件", path)
	}

	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// -hide_banner 去掉版本横幅;-nostdin 防止 ffmpeg 抢走终端输入。
	cmd := exec.CommandContext(cctx, r.Path, "-hide_banner", "-nostdin", "-i", abs)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = nil

	runErr := cmd.Run()
	out := stderr.String()

	if r.Verbose {
		fmt.Fprintf(os.Stderr, "--- ffmpeg -i %s ---\n%s\n", abs, out)
	}

	// 退出码非 0 是预期的(未指定输出文件),但上下文超时/被取消是真错误。
	if cctx.Err() != nil {
		return nil, fmt.Errorf("探测 %s 超时: %w", path, cctx.Err())
	}

	// ffmpeg 起不来:常见原因是路径写错、没有可执行权限或从未下载成功。
	// 用"没有任何输出"来判断比只认 exec.ErrNotFound 更稳(绝对路径场景返回的是 PathError)。
	if runErr != nil {
		if errors.Is(runErr, exec.ErrNotFound) {
			return nil, fmt.Errorf("找不到 ffmpeg 可执行文件: %s", r.Path)
		}
		if strings.TrimSpace(out) == "" {
			return nil, fmt.Errorf("无法执行 ffmpeg(%s): %w —— 请确认文件存在且有可执行权限", r.Path, runErr)
		}
	}

	if reInvalidData.MatchString(out) || strings.Contains(out, "Error opening input") {
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, path)
	}

	res := &ProbeResult{Path: abs}

	// 解析容器名: `Input #0, mov,mp4,m4a,3gp,3g2,mj2, from 'x.mp4':`
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Input #") {
			if i := strings.Index(line, ", from "); i > 0 {
				res.Container = strings.TrimPrefix(line[:i], "Input #0, ")
				break
			}
		}
	}

	if m := reDuration.FindStringSubmatch(out); m != nil {
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		sec, _ := strconv.ParseFloat(m[3], 64)
		start, _ := strconv.ParseFloat(m[4], 64)
		bitrate, _ := strconv.Atoi(m[5])

		res.Duration = time.Duration((float64(h*3600+min*60) + sec) * float64(time.Second))
		res.Start = time.Duration(start * float64(time.Second))
		res.Bitrate = bitrate
		res.RawDuration = fmt.Sprintf("%02d:%02d:%s", h, min, m[3])
	} else {
		return nil, fmt.Errorf("无法解析 %s 的时长信息(可能不是有效的媒体文件)", path)
	}

	for _, m := range reStream.FindAllStringSubmatch(out, -1) {
		// m[2] 是 #0:N 中的 N,即流在输入内的索引。
		idx, _ := strconv.Atoi(m[2])
		kind := strings.ToLower(m[3])
		st := Stream{Index: idx, Kind: kind, Codec: m[4], Detail: strings.TrimSpace(m[0])}
		res.Streams = append(res.Streams, st)
		switch kind {
		case "video":
			res.HasVideo = true
		case "audio":
			res.HasAudio = true
		}
	}

	if res.Duration <= 0 {
		return nil, fmt.Errorf("%s 的时长为 0,无法裁剪", path)
	}
	return res, nil
}

// reDebugTSVideo 匹配 `-debug_ts` 输出的首个视频包所在行,捕获其中的 DTS:
//
//	[vist#0:0/hevc @ 0x...] demuxer -> ist_index:0:0 type:video pkt_pts:7204140 pkt_pts_time:80.046 pkt_dts:7196940 pkt_dts_time:79.966 ...
//
// 注意 `type:video` 之后先跟 `pkt_pts:<整数>`,再才是 `pkt_pts_time:<浮点>`、`pkt_dts:*`,中间不能漏。
//
// 为什么取 DTS 而不是 PTS:含 B 帧的流里首个视频包的 PTS 会晚于它的 DTS(差一个
// B 帧时长),而 ffmpeg 搬移/对齐时间是按 DTS 走的。实测用 PTS 算出的补偿量会差
// 一个 B 帧(如 0.08s 残留),用 DTS 才能把视频流起点精确归零。
var reDebugTSVideo = regexp.MustCompile(`demuxer -> ist_index:\d+:\d+ type:video pkt_pts:-?\d+ pkt_pts_time:-?[\d.]+ pkt_dts:-?\d+ pkt_dts_time:(-?[\d.]+)`)

// VideoKeyframeStart 探测"从 start 定位时,ffmpeg 实际采用的视频起点时间戳(DTS)"。
//
// 背景:输出侧 `-ss <start>` 会向前回溯到 <= start 的最近关键帧开始搬运,但输出的
// 时间戳基线仍按 start 计算,导致产物视频流从一个 >0 的时间点才开始(开头出现
// "只有音频没有画面"的空档)。关键帧间隔越大空档越长 —— 10 秒 GOP 就是开头 10 秒黑屏,
// 手机/系统播放器会直接判定文件损坏。调用方据此用 -itsoffset 把这段差值补偿掉。
//
// ffmpeg-trim 没有 ffprobe,这里用 `-debug_ts` 读首个视频包的 demuxer DTS:
//
//	ffmpeg -ss <start> -i <file> -t 0.05 -map 0 -c copy -f null - -debug_ts
//
// 返回 (实际视频起点 DTS, 是否存在视频流, error)。
// 无视频流(如纯音频)时返回 ok=false 且 err=nil,调用方应跳过补偿。
func (r *Runner) VideoKeyframeStart(ctx context.Context, path string, start time.Duration) (time.Duration, bool, error) {
	// 起点为 0 时不会发生回溯,无需探测。
	if start <= 0 {
		return 0, true, nil
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, false, err
	}

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// -t 0.05:只需读到第一个视频包;-f null -:不产出真实文件。
	// -debug_ts 会把每个包的时间戳打到 stderr。注意 -map 0/-c copy 保持与真实裁剪一致,
	// 否则 ffmpeg 选流策略可能不同,探测到的起点与裁剪时不一致。
	cmd := exec.CommandContext(cctx, r.Path,
		"-hide_banner", "-nostdin",
		"-ss", formatSeconds(start),
		"-i", abs,
		"-t", "0.05",
		"-map", "0",
		"-c", "copy",
		"-f", "null", "-",
		"-debug_ts",
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = nil

	// 退出码可能非 0(如部分容器写 null 时的告警),只要 stderr 里有可用行就算成功。
	_ = cmd.Run()
	out := stderr.String()

	if r.Verbose {
		fmt.Fprintf(os.Stderr, "--- ffmpeg -debug_ts (keyframe probe) %s @ %s ---\n%s\n", abs, formatSeconds(start), out)
	}

	m := reDebugTSVideo.FindStringSubmatch(out)
	if m == nil {
		// 没有视频包:可能是纯音频,也可能是探测失败。交由调用方决定如何处理。
		return 0, false, nil
	}
	sec, perr := strconv.ParseFloat(m[1], 64)
	if perr != nil {
		return 0, false, fmt.Errorf("解析关键帧时间戳失败 %q: %w", m[1], perr)
	}
	if sec < 0 {
		sec = 0
	}
	return time.Duration(sec * float64(time.Second)), true, nil
}

// formatSeconds 把时长格式化成 ffmpeg 接受的高精度秒数(与 trim 包保持一致的格式)。
func formatSeconds(d time.Duration) string {
	return fmt.Sprintf("%.3f", d.Seconds())
}

// PrintSummary 生成人类可读的探测摘要。
func (p *ProbeResult) PrintSummary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", filepath.Base(p.Path))
	fmt.Fprintf(&b, "  容器: %s\n", p.Container)
	fmt.Fprintf(&b, "  时长: %s (%.3fs)\n", p.RawDuration, p.DurationSeconds())
	if p.Bitrate > 0 {
		fmt.Fprintf(&b, "  码率: %d kb/s\n", p.Bitrate)
	}
	for _, s := range p.Streams {
		fmt.Fprintf(&b, "  流#%d: %s / %s\n", s.Index, s.Kind, s.Codec)
	}
	return b.String()
}

// SupportsExt 判断扩展名是否在支持的容器白名单内(大小写不敏感)。
func SupportsExt(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	for _, e := range SupportedExts {
		if ext == e {
			return true
		}
	}
	return false
}
