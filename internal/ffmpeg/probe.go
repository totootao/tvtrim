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
var SupportedExts = []string{".mp4", ".mkv", ".ts", ".avi", ".mp3"}

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

	// ffmpeg 找不到时给出可读的提示。
	if runErr != nil && errors.Is(runErr, exec.ErrNotFound) {
		return nil, fmt.Errorf("找不到 ffmpeg 可执行文件: %s", r.Path)
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
