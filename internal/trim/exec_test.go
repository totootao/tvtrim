package trim

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/totootao/tvtrim/internal/ffmpeg"
	"github.com/totootao/tvtrim/internal/scan"
	"github.com/totootao/tvtrim/internal/testkit"
)

const probeStderr = `Input #0, mov,mp4,m4a,3gp,3g2,mj2, from '%s':
  Duration: 00:20:00.00, start: 0.000000, bitrate: 500 kb/s
  Stream #0:0[0x1](und): Video: h264 (avc1 / 0x31637661), yuv420p, 1920x1080, 400 kb/s, 25 fps
  Stream #0:1[0x2](und): Audio: aac (mp4a / 0x6134706D), 44100 Hz, stereo, fltp, 128 kb/s
`

// newFakeRunner 返回用假 ffmpeg 驱动的 Runner:
// 同一份脚本按参数自动区分"探测"(输出样本 stderr)与"裁剪"(写出文件)。
func newFakeRunner(t *testing.T, stderrSample, mode string) *ffmpeg.Runner {
	t.Helper()
	dir := t.TempDir()
	if stderrSample != "" {
		out := testkit.WriteFile(t, dir, "stderr.txt", stderrSample)
		testkit.SetEnv(t, testkit.EnvOut, out)
	}
	if mode != "" {
		testkit.SetEnv(t, testkit.EnvMode, mode)
	}
	return &ffmpeg.Runner{Path: testkit.WriteFakeFFmpeg(t, dir)}
}

// probeStderrOf 生成指定路径的 ffmpeg -i 输出样本(20 分钟片)。
func probeStderrOf(path string) string {
	return "Input #0, mov,mp4,m4a,3gp,3g2,mj2, from '" + path + "':\n" +
		"  Duration: 00:20:00.00, start: 0.000000, bitrate: 500 kb/s\n" +
		"  Stream #0:0[0x1](und): Video: h264 (avc1 / 0x31637661), yuv420p, 1920x1080, 25 fps\n" +
		"  Stream #0:1[0x2](und): Audio: aac (mp4a / 0x6134706D), 44100 Hz, stereo, fltp, 128 kb/s\n"
}

// probeOf 用假 ffmpeg 探测一个文件,得到用于 BuildPlan 的 ProbeResult。
func probeOf(t *testing.T, path string, secs float64) *ffmpeg.ProbeResult {
	t.Helper()
	return &ffmpeg.ProbeResult{
		Path: path, Container: "mov,mp4",
		Duration: time.Duration(secs * float64(time.Second)),
		HasVideo: true, HasAudio: true,
	}
}

func TestBuildPlanHeadOnly(t *testing.T) {
	p := probeOf(t, "/v/a.mp4", 1200)
	plan := BuildPlan(p, Options{Head: 90 * time.Second})
	if plan.Start != 90*time.Second || plan.End != 1200*time.Second {
		t.Errorf("start/end = %v/%v", plan.Start, plan.End)
	}
	if plan.Skip {
		t.Errorf("不应跳过: %s", plan.SkipWhy)
	}
}

func TestBuildPlanTailExceedsDuration(t *testing.T) {
	// 片尾时长超过总时长 → 必须跳过而不是产生负 -to。
	p := probeOf(t, "/v/a.mp4", 100)
	plan := BuildPlan(p, Options{Tail: 150 * time.Second})
	if !plan.Skip {
		t.Error("头尾超过总时长时应跳过")
	}
	if !strings.Contains(plan.SkipWhy, "超过") {
		t.Errorf("跳过原因不符: %s", plan.SkipWhy)
	}
}

func TestBuildPlanNoTrimRequested(t *testing.T) {
	p := probeOf(t, "/v/a.mp4", 600)
	plan := BuildPlan(p, Options{})
	if !plan.Skip || plan.SkipWhy == "" {
		t.Error("未指定头尾时应跳过并给出原因")
	}
}

func TestBuildPlanHeadBeyondDurationClamped(t *testing.T) {
	p := probeOf(t, "/v/a.mp4", 60)
	plan := BuildPlan(p, Options{Head: 200 * time.Second, Tail: 0})
	if plan.Start != 60*time.Second {
		t.Errorf("超出范围时应钳制到总时长,得到 %v", plan.Start)
	}
}

func TestMuxerForAllExtensions(t *testing.T) {
	cases := []struct{ path, want string }{
		{"a.mp4", "mp4"}, {"a.m4v", "mp4"}, {"a.mov", "mp4"},
		{"a.mkv", "matroska"}, {"a.ts", "mpegts"}, {"a.avi", "avi"},
		{"a.mp3", "mp3"}, {"a.webm", "webm"}, {"a.asf", "asf"}, {"a.wmv", "asf"},
		{"a.m4v.xxx", "matroska"}, // 未知扩展名回退 matroska
	}
	for _, c := range cases {
		if got := muxerFor(c.path); got != c.want {
			t.Errorf("muxerFor(%q) = %q, 期望 %q", c.path, got, c.want)
		}
	}
}

func TestOutputPathVariants(t *testing.T) {
	cases := []struct {
		input string
		opts  Options
		want  string
	}{
		{"/d/a.mp4", Options{Suffix: "-trim"}, "/d/a-trim.mp4"},
		{"/d/a.mp4", Options{}, "/d/a-trim.mp4"}, // 默认后缀
		{"/d/a.mkv", Options{Suffix: "_cut"}, "/d/a_cut.mkv"},
		{"/d/a.mkv", Options{Output: "/out/b.mkv"}, "/out/b.mkv"},
		{"/d/a.ts", Options{Suffix: "inplace"}, "/d/a.ts"}, // 原地
	}
	for _, c := range cases {
		if got := OutputPath(c.input, c.opts); got != c.want {
			t.Errorf("OutputPath(%q) = %q, 期望 %q", c.input, got, c.want)
		}
	}
}

func TestPlanWindowSeconds(t *testing.T) {
	p := probeOf(t, "/v/a.mp4", 600)
	plan := BuildPlan(p, Options{Head: 30 * time.Second, Tail: 60 * time.Second})
	s, e := plan.WindowSeconds()
	if s != 30 || e != 540 {
		t.Errorf("WindowSeconds = (%v,%v), 期望 (30,540)", s, e)
	}
}

// TestExecutorCopyMode 验证 copy 成功时的输出与体积统计。
func TestExecutorCopyMode(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRunner(t, "", "")
	in := testkit.WriteFile(t, dir, "a.mp4", "x")
	p := probeOf(t, in, 600)
	plan := BuildPlan(p, Options{Head: 10 * time.Second, Tail: 10 * time.Second})
	plan.Output = filepath.Join(dir, "out.mp4")

	ex := &Executor{FFmpeg: r, Opts: Options{Head: 10 * time.Second, Tail: 10 * time.Second}}
	out, err := ex.Run(context.Background(), plan)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != plan.Output {
		t.Errorf("输出路径 = %q, 期望 %q", out, plan.Output)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("输出文件不存在: %v", err)
	}
}

// TestExecutorEmptyOutput 验证 ffmpeg 退出码 0 但产出空文件时的检测。
func TestExecutorEmptyOutput(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRunner(t, "", testkit.ModeEmpty)
	in := testkit.WriteFile(t, dir, "a.mp4", "x")
	p := probeOf(t, in, 600)
	plan := BuildPlan(p, Options{Head: 10 * time.Second, Tail: 10 * time.Second})
	plan.Output = filepath.Join(dir, "empty.mp4")

	ex := &Executor{FFmpeg: r, Opts: Options{Head: 10 * time.Second}}
	if _, err := ex.Run(context.Background(), plan); err == nil {
		t.Fatal("空输出应报错")
	} else if !strings.Contains(err.Error(), "疑似空文件") {
		t.Errorf("错误信息不符: %v", err)
	}
	if _, err := os.Stat(plan.Output); !os.IsNotExist(err) {
		t.Error("空输出文件应被删除")
	}
}

// TestExecutorFailMode 验证 ffmpeg 失败时返回可读错误。
func TestExecutorFailMode(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRunner(t, "", testkit.ModeFail)
	in := testkit.WriteFile(t, dir, "a.mp4", "x")
	p := probeOf(t, in, 600)
	plan := BuildPlan(p, Options{Head: 10 * time.Second})
	plan.Output = filepath.Join(dir, "fail.mp4")

	ex := &Executor{FFmpeg: r, Opts: Options{Head: 10 * time.Second}}
	if _, err := ex.Run(context.Background(), plan); err == nil {
		t.Fatal("ffmpeg 失败应返回错误")
	}
}

// TestExecutorOutputExists 验证默认拒绝覆盖。
func TestExecutorOutputExists(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRunner(t, "", "")
	in := testkit.WriteFile(t, dir, "a.mp4", "x")
	out := testkit.WriteFile(t, dir, "exists.mp4", "already")
	p := probeOf(t, in, 600)
	plan := BuildPlan(p, Options{Head: 10 * time.Second})
	plan.Output = out // 故意让输出文件名保
	plan.Input = in

	ex := &Executor{FFmpeg: r, Opts: Options{Head: 10 * time.Second}}
	if _, err := ex.Run(context.Background(), plan); err == nil {
		t.Fatal("已存在的输出应被拒绝")
	} else if !strings.Contains(err.Error(), "已存在") {
		t.Errorf("错误信息不符: %v", err)
	}
}

// TestRunBatchHeadTailOverride 验证逐文件头尾覆盖(自动识别/网页模式依赖)。
func TestRunBatchHeadTailOverride(t *testing.T) {
	dir := t.TempDir()
	// 假 ffmpeg 探测阶段需要针对每个文件输出对应的 stderr 样本。
	var items []scan.Item
	for _, n := range []string{"ep01.mp4", "ep02.mp4"} {
		p := testkit.WriteFile(t, dir, n, "x")
		items = append(items, scan.Item{Path: p, Size: 1024})
	}
	r := newFakeRunner(t, probeStderrOf(items[0].Path), "")

	// 统一 head=10s;ep02 覆盖为 head=20s tail=5s。
	override := map[string]HeadTail{
		items[1].Path: {Head: 20 * time.Second, Tail: 5 * time.Second},
	}
	var got []HeadTail
	res := RunBatch(context.Background(), r, items, BatchOptions{
		Trim:             Options{Head: 10 * time.Second, Tail: 5 * time.Second, MinDuration: time.Second, Suffix: "-trim"},
		Workers:          1,
		HeadTailOverride: override,
		OnResult: func(r Result) {
			got = append(got, HeadTail{Head: r.Plan.Head, Tail: r.Plan.Tail})
		},
	})
	if res.Failed != 0 {
		t.Fatalf("不应有失败: %+v", res.Results)
	}
	if len(got) != 2 {
		t.Fatalf("OnResult 回调次数 = %d, 期望 2", len(got))
	}
	// 结果顺序不保证,逐个核对:应出现 (10,5) 与 (20,5) 各一次。
	seen := map[HeadTail]int{}
	for _, h := range got {
		seen[h]++
	}
	if seen[HeadTail{Head: 10 * time.Second, Tail: 5 * time.Second}] != 1 {
		t.Errorf("未命中默认 head/tail 的结果: %+v", got)
	}
	if seen[HeadTail{Head: 20 * time.Second, Tail: 5 * time.Second}] != 1 {
		t.Errorf("未命中覆盖 head/tail 的结果: %+v", got)
	}
}

// TestRunBatchEmptyList 验证空输入不炸。
func TestRunBatchEmptyList(t *testing.T) {
	r := newFakeRunner(t, "", "")
	res := RunBatch(context.Background(), r, nil, BatchOptions{Trim: Options{Head: time.Second}})
	if res.Total != 0 || res.Done != 0 || res.Failed != 0 {
		t.Errorf("空列表结果异常: %+v", res)
	}
}
