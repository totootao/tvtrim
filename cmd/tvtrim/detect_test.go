package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/totootao/tvtrim/internal/auto"
	"github.com/totootao/tvtrim/internal/ffmpeg"
	"github.com/totootao/tvtrim/internal/scan"
	"github.com/totootao/tvtrim/internal/testkit"
	"github.com/totootao/tvtrim/internal/trim"
)

// combinedStderr 是一份"探测 + 静音检测"合一的 ffmpeg stderr 样本:
// 20 分钟片子,用户">{{0-2}} 黑屏 / 95-96.5 转场静音 / 1100-1101.5 转折 / 1135-1200 ED 后静音。
// 假 ffmpeg 在 probe 与 silencedetect 两种调用下都输出它,便于驱动完整链路。
const combinedStderr = `ffmpeg version n6.0-trim Copyright (c) 2000-2023 the FFmpeg developers
Input #0, mov,mp4,m4a,3gp,3g2,mj2, from 'in.mp4':
  Metadata:
    major_brand     : isom
  Duration: 00:20:00.00, start: 0.000000, bitrate: 1843 kb/s
  Stream #0:0[0x1](und): Video: h264 (avc1 / 0x31637661), yuv420p, 1920x1080, 1700 kb/s, 25 fps
  Stream #0:1[0x2](und): Audio: aac (mp4a / 0x6134706D), 44100 Hz, stereo, fltp, 128 kb/s
[silencedetect @ 0x55b] silence_start: 0
[silencedetect @ 0x55b] silence_end: 2 | silence_duration: 2
[silencedetect @ 0x55b] silence_start: 95
[silencedetect @ 0x55b] silence_end: 96.5 | silence_duration: 1.5
[silencedetect @ 0x55b] silence_start: 1100
[silencedetect @ 0x55b] silence_end: 1101.5 | silence_duration: 1.5
[silencedetect @ 0x55b] silence_start: 1135
[silencedetect @ 0x55b] silence_end: 1200 | silence_duration: 65
At least one output file must be specified
`

// newRunner 造一个假 ffmpeg Runner,并把样本文件挂到环境变量上。
func newRunner(t *testing.T) *ffmpeg.Runner {
	t.Helper()
	root := t.TempDir()
	ff := testkit.WriteFakeFFmpeg(t, root)
	testkit.SetEnv(t, testkit.EnvOut, testkit.WriteFile(t, root, "stderr.txt", combinedStderr))
	return &ffmpeg.Runner{Path: ff}
}

// sampleItems 在临时目录里造 n 个真实的剧集文件(内容不重要,存在即可)。
func sampleItems(t *testing.T, n int) []scan.Item {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "S01")
	var items []scan.Item
	for i := 1; i <= n; i++ {
		p := testkit.WriteFile(t, dir, fmt.Sprintf("S01E%02d.mkv", i), "fake-media")
		items = append(items, scan.Item{Path: p, Size: 1024, Season: 1, Episode: i})
	}
	return items
}

func TestDetectResultOK(t *testing.T) {
	okItem := detectResult{Res: auto.Result{OK: true}}
	if !okItem.OK() {
		t.Error("识别成功的结果 OK() 应为 true")
	}
	if (detectResult{Res: auto.Result{OK: true}, Err: os.ErrNotExist}).OK() {
		t.Error("有错误时 OK() 应为 false")
	}
	if (detectResult{}).OK() {
		t.Error("空结果 OK() 应为 false")
	}
}

func TestDetectAllProbeOnly(t *testing.T) {
	runner := newRunner(t)
	items := sampleItems(t, 2)

	got := detectAll(context.Background(), runner, items, 2, DetectProbeOnly)
	if len(got) != 2 {
		t.Fatalf("结果数 = %d, 期望 2", len(got))
	}
	// 结果按路径排序,便于稳定展示。
	for i := 1; i < len(got); i++ {
		if got[i-1].Item.Path > got[i].Item.Path {
			t.Fatalf("结果未按路径排序: %s > %s", got[i-1].Item.Path, got[i].Item.Path)
		}
	}
	for _, d := range got {
		if d.Err != nil {
			t.Fatalf("探测失败: %v", d.Err)
		}
		if d.Probe == nil {
			t.Fatal("Probe 应被填充")
		}
		if got := d.Probe.Duration; got != 20*time.Minute {
			t.Errorf("时长 = %v, 期望 20m", got)
		}
		if d.OK() {
			t.Error("probe-only 模式没有识别结果,OK() 应为 false")
		}
	}
}

func TestDetectAllSilenceWithModeCorrection(t *testing.T) {
	runner := newRunner(t)
	items := sampleItems(t, 3)

	got := detectAll(context.Background(), runner, items, 0, DetectSilence) // workers=0 → 自动取核数
	if len(got) != 3 {
		t.Fatalf("结果数 = %d, 期望 3", len(got))
	}
	for _, d := range got {
		if !d.OK() {
			t.Fatalf("%s 识别失败: err=%v res=%+v", d.Item.Path, d.Err, d.Res)
		}
		if d.Res.Head != 96.5 || d.Res.Tail != 100 {
			t.Errorf("%s 切点 = head %.2f / tail %.2f, 期望 96.5 / 100",
				d.Item.Path, d.Res.Head, d.Res.Tail)
		}
		// 多集时会附加快照一致率。
		if !strings.HasPrefix(d.Res.Note, "[一致 ") {
			t.Errorf("%s Note 应带一致率前缀,实际 %q", d.Item.Path, d.Res.Note)
		}
	}
}

// 众数纠错应把离群的那一集拉回多数派的切点。
func TestApplyModeCorrectionFixesOutlier(t *testing.T) {
	results := []detectResult{
		{Item: scan.Item{Path: "e1"}, Res: auto.Result{OK: true, Head: 96.5, Tail: 100}},
		{Item: scan.Item{Path: "e2"}, Res: auto.Result{OK: true, Head: 96.5, Tail: 100}},
		{Item: scan.Item{Path: "e3"}, Res: auto.Result{OK: true, Head: 88.0, Tail: 100}},
		{Item: scan.Item{Path: "e4"}, Err: os.ErrInvalid},
	}
	applyModeCorrection(results)
	if results[2].Res.Head != 96.5 {
		t.Errorf("离群集应被修正为 96.5,实际 %.2f", results[2].Res.Head)
	}
	if !results[2].Res.Fixed {
		t.Error("离群集应标记 Fixed")
	}
	if results[0].Res.Fixed {
		t.Error("多数集不应被标记 Fixed")
	}
	// Err 非空的结果不应被纠错改写。
	if results[3].Res.OK {
		t.Error("失败项不应被纠错成可用")
	}
}

func TestEffectiveWorkers(t *testing.T) {
	cases := []struct {
		name       string
		workers, n int
		wantMin    int
		wantMax    int
		want       int
		exact      bool
	}{
		{"负值与 0 退化为任务数", 0, 3, 1, 3, 0, false},
		{"并发数超过任务数时收敛", 8, 4, 4, 4, 4, true},
		{"用户指定优先", 2, 4, 2, 2, 2, true},
		{"任务数为 0 时不收缩", 4, 0, 4, 4, 4, true},
		{"兜底至少 1", -1, 1, 1, 1, 1, true},
	}
	for _, c := range cases {
		got := effectiveWorkers(c.workers, c.n)
		if c.exact {
			if got != c.want {
				t.Errorf("%s: = %d, 期望 %d", c.name, got, c.want)
			}
			continue
		}
		if got < c.wantMin || got > c.wantMax {
			t.Errorf("%s: = %d, 期望落在 [%d,%d]", c.name, got, c.wantMin, c.wantMax)
		}
	}
}

func TestHeadTailOverride(t *testing.T) {
	results := []detectResult{
		{Item: scan.Item{Path: "/a.mkv"}, Res: auto.Result{OK: true, Head: 30, Tail: 20}},
		{Item: scan.Item{Path: "/b.mkv"}, Res: auto.Result{OK: true, Head: 25.5, Tail: 19.75}},
		{Item: scan.Item{Path: "/c.mkv"}, Res: auto.Result{OK: false}},
	}
	m := headTailOverride(results)
	if len(m) != 2 {
		t.Fatalf("覆盖表应有 2 项(失败项不参与),实际 %d", len(m))
	}
	if m["/a.mkv"].Head != 30*time.Second || m["/a.mkv"].Tail != 20*time.Second {
		t.Errorf("a head/tail = %v/%v", m["/a.mkv"].Head, m["/a.mkv"].Tail)
	}
	if got := m["/b.mkv"].Head; got != 25500*time.Millisecond {
		t.Errorf("b head = %v, 期望 25.5s", got)
	}
	if _, ok := m["/c.mkv"]; ok {
		t.Error("失败项不应出现在覆盖表里")
	}
}

func TestFailCountAndRunnableItems(t *testing.T) {
	results := []detectResult{
		{Item: scan.Item{Path: "/a"}, Res: auto.Result{OK: true}},
		{Item: scan.Item{Path: "/b"}, Res: auto.Result{OK: false}},
		{Item: scan.Item{Path: "/c"}, Err: os.ErrClosed},
		{Item: scan.Item{Path: "/d"}, Res: auto.Result{OK: true}},
	}
	if got := failCount(results); got != 2 {
		t.Errorf("failCount = %d, 期望 2", got)
	}
	items := runnableItems(results)
	if len(items) != 2 || items[0].Path != "/a" || items[1].Path != "/d" {
		t.Errorf("runnableItems = %+v, 期望 a/d", items)
	}
	if got := runnableItems(nil); len(got) != 0 {
		t.Errorf("空输入应返回空列表,实际 %d", len(got))
	}
}

// detectResult → trim.HeadTail 的秒数换算精度。(断言 +1s 内)
func TestHeadTailSecondPrecision(t *testing.T) {
	m := headTailOverride([]detectResult{
		{Item: scan.Item{Path: "/a"}, Res: auto.Result{OK: true, Head: 21.5, Tail: 19.99}},
	})
	if got := m["/a"].Head.Seconds(); got < 21.4 || got > 21.6 {
		t.Errorf("head = %v, 期望 ~21.5s", got)
	}
	if got := m["/a"].Tail.Seconds(); got < 19.9 || got > 20.1 {
		t.Errorf("tail = %v, 期望 ~19.99s", got)
	}
	var _ trim.HeadTail = m["/a"]
}
