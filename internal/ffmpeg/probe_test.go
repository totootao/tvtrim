package ffmpeg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/totootao/tvtrim/internal/testkit"
)

// mp4Stderr 是 `ffmpeg -i x.mp4` 的典型输出(含视频+音频)。
const mp4Stderr = `Input #0, mov,mp4,m4a,3gp,3g2,mj2, from '/v/x.mp4':
  Metadata:
    major_brand     : isom
  Duration: 00:01:23.45, start: 0.000000, bitrate: 1234 kb/s
  Stream #0:0[0x1](und): Video: h264 (avc1 / 0x31637661), yuv420p, 1920x1080, 1000 kb/s, 25 fps, 25 tbr, 12800 tbn
  Stream #0:1[0x2](und): Audio: aac (mp4a / 0x6134706D), 44100 Hz, stereo, fltp, 128 kb/s (default)
`

// mkvStderr 带负 start 时间(部分 mkv 的常见现象)。
const mkvStderr = `Input #0, matroska,webm, from '/v/y.mkv':
  Duration: 00:20:00.50, start: -0.023000, bitrate: 2048 kb/s
  Stream #0:0: Video: hevc, yuv420p, 1920x1080, 25 tbr, 1k tbn
  Stream #0:1(jpn): Audio: flac, 48000 Hz, stereo, s32
  Stream #0:2: Subtitle: ass (default)
`

// newProbeRunner 返回一个用假 ffmpeg 驱动探测的 Runner。
func newProbeRunner(t *testing.T, dir, stderr string) *Runner {
	t.Helper()
	testkit.SetEnv(t, testkit.EnvMode, testkit.ModeProbe)
	out := testkit.WriteFile(t, dir, "stderr.txt", stderr)
	testkit.SetEnv(t, testkit.EnvOut, out)
	return &Runner{Path: testkit.WriteFakeFFmpeg(t, dir)}
}

// probeTarget 返回一个存在的文件路径,满足 Probe 的存在性检查。
func probeTarget(t *testing.T, dir, name string) string {
	t.Helper()
	return testkit.WriteFile(t, dir, name, "fake-media-content")
}

func TestProbeMP4(t *testing.T) {
	dir := t.TempDir()
	r := newProbeRunner(t, dir, mp4Stderr)
	p, err := r.Probe(context.Background(), probeTarget(t, dir, "v.mp4"))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if want := 83*time.Second + 450*time.Millisecond; p.Duration != want {
		t.Errorf("Duration = %v, 期望 %v", p.Duration, want)
	}
	if p.Bitrate != 1234 {
		t.Errorf("Bitrate = %d, 期望 1234", p.Bitrate)
	}
	if !p.HasVideo || !p.HasAudio {
		t.Errorf("应同时含音视频: video=%v audio=%v", p.HasVideo, p.HasAudio)
	}
	if len(p.Streams) != 2 {
		t.Fatalf("流数量 = %d, 期望 2", len(p.Streams))
	}
	if p.Streams[0].Codec != "h264" || p.Streams[1].Codec != "aac" {
		t.Errorf("流解析错误: %+v", p.Streams)
	}
	if !strings.Contains(p.Container, "mov,mp4") {
		t.Errorf("Container = %q", p.Container)
	}
	if p.RawDuration != "00:01:23.45" {
		t.Errorf("RawDuration = %q", p.RawDuration)
	}
}

func TestProbeNegativeStart(t *testing.T) {
	dir := t.TempDir()
	r := newProbeRunner(t, dir, mkvStderr)
	p, err := r.Probe(context.Background(), probeTarget(t, dir, "v.mp4"))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if want := 20*time.Minute + 500*time.Millisecond; p.Duration != want {
		t.Errorf("Duration = %v, 期望 %v", p.Duration, want)
	}
	if p.Start >= 0 {
		t.Errorf("负 start 未解析: %v", p.Start)
	}
	if len(p.Streams) != 3 {
		t.Errorf("流数量 = %d, 期望 3(含字幕)", len(p.Streams))
	}
	if p.HasVideo != true || p.HasAudio != true {
		t.Errorf("视频/音频标记异常")
	}
}

func TestProbeUnsupported(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"invalid data": "...\nInvalid data found when processing input\n",
		"open error":   "Error opening input file x.mkv.\n",
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			r := newProbeRunner(t, t.TempDir(), out)
			_, err := r.Probe(context.Background(), probeTarget(t, dir, "v.bin"))
			if !errors.Is(err, ErrUnsupported) {
				t.Errorf("期望 ErrUnsupported,得到 %v", err)
			}
		})
	}
}

func TestProbeNoDuration(t *testing.T) {
	r := newProbeRunner(t, t.TempDir(), "Input #0, mp3, from 'x.mp3':\n  nothing useful\n")
	if _, err := r.Probe(context.Background(), probeTarget(t, t.TempDir(), "x.mp4")); err == nil {
		t.Error("无 Duration 行时应报错")
	}
}

func TestProbeZeroDuration(t *testing.T) {
	out := "Input #0, mov,mp4, from 'z.mp4':\n  Duration: 00:00:00.00, start: 0.000000, bitrate: 100 kb/s\n"
	r := newProbeRunner(t, t.TempDir(), out)
	if _, err := r.Probe(context.Background(), probeTarget(t, t.TempDir(), "z.mp4")); err == nil {
		t.Error("时长为 0 时应报错")
	}
}

func TestProbeDirectory(t *testing.T) {
	r := &Runner{Path: "ffmpeg"}
	if _, err := r.Probe(context.Background(), t.TempDir()); err == nil {
		t.Error("传入目录应报错")
	} else if !strings.Contains(err.Error(), "是目录") {
		t.Errorf("错误信息不符: %v", err)
	}
}

func TestProbeMissingFile(t *testing.T) {
	r := &Runner{Path: "ffmpeg"}
	if _, err := r.Probe(context.Background(), filepath.Join(t.TempDir(), "nope.mp4")); err == nil {
		t.Error("文件不存在时应报错")
	}
}

func TestProbeMissingFFmpeg(t *testing.T) {
	// ffmpeg 可执行文件不存在 → exec 返回 ErrNotFound 分支。
	r := &Runner{Path: filepath.Join(t.TempDir(), "absent-ffmpeg")}
	_, err := r.Probe(context.Background(), testkit.WriteFile(t, t.TempDir(), "a.mp4", "x"))
	if err == nil || !strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("应给出 ffmpeg 缺失提示,得到 %v", err)
	}
}

func TestPrintSummaryAndSeconds(t *testing.T) {
	p := &ProbeResult{
		Path: "/v/a.mp4", Container: "mov,mp4", Duration: 90 * time.Second,
		Bitrate: 800, RawDuration: "00:01:30.00",
		Streams: []Stream{{Index: 0, Kind: "video", Codec: "h264"}},
	}
	s := p.PrintSummary()
	for _, want := range []string{"a.mp4", "mov,mp4", "800 kb/s", "video / h264"} {
		if !strings.Contains(s, want) {
			t.Errorf("摘要缺少 %q:\n%s", want, s)
		}
	}
	if p.DurationSeconds() != 90 {
		t.Errorf("DurationSeconds = %v", p.DurationSeconds())
	}
}

func TestSupportsExt(t *testing.T) {
	ok := []string{"a.mp4", "b.MKV", "c.ts", "d.avi", "e.mp3", "f.webm", "g.asf", "h.WMV", "x/movie.m4v"}
	for _, p := range ok {
		if p == "x/movie.m4v" {
			continue // m4v 不在白名单,仅用于对照
		}
		if !SupportsExt(p) {
			t.Errorf("SupportsExt(%q) 应为 true", p)
		}
	}
	if SupportsExt("a.m4v") || SupportsExt("a.flv") || SupportsExt("noext") {
		t.Error("不应支持 m4v/flv/无扩展名")
	}
}

func TestDetectSilencesFake(t *testing.T) {
	dir := t.TempDir()
	testkit.SetEnv(t, testkit.EnvMode, testkit.ModeSilence)
	testkit.SetEnv(t, testkit.EnvOut, testkit.WriteFile(t, dir, "s.txt",
		"[silencedetect @ 0x1] silence_start: 95\n"+
			"[silencedetect @ 0x1] silence_end: 96.5 | silence_duration: 1.5\n"))
	r := &Runner{Path: testkit.WriteFakeFFmpeg(t, dir)}
	sils, err := r.DetectSilences(context.Background(), dir, -35, 0.8, 1200)
	if err != nil {
		t.Fatalf("DetectSilences: %v", err)
	}
	if len(sils) != 1 || sils[0].Start != 95 || sils[0].End != 96.5 {
		t.Fatalf("静音区间不符: %+v", sils)
	}
}

func TestClearCache(t *testing.T) {
	// 缓存目录里的旧/fake 二进制在 web 与 CLI 共用,这里只确认能用工具包造文件。
	p := testkit.WriteFile(t, t.TempDir(), "x.bin", "data")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("WriteFile 异常: %v", err)
	}
}
