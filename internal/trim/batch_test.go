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

// TestPlansProbesEveryItem 验证 dry-run 预览会为每个文件算出计划。
func TestPlansProbesEveryItem(t *testing.T) {
	dir := t.TempDir()
	var items []scan.Item
	for _, n := range []string{"a.mkv", "b.mkv"} {
		p := testkit.WriteFile(t, dir, n, "x")
		items = append(items, scan.Item{Path: p, Size: 512})
	}
	r := newFakeRunner(t, probeStderrOf(items[0].Path), "")

	plans := Plans(context.Background(), r, items, Options{Head: 30 * time.Second, Tail: 40 * time.Second}, 0)
	if len(plans) != 2 {
		t.Fatalf("计划数 = %d, 期望 2", len(plans))
	}
	for _, p := range plans {
		if p.Skip {
			t.Errorf("%s 不应跳过: %s", p.Input, p.SkipWhy)
		}
		if p.Start != 30*time.Second {
			t.Errorf("%s 起点 = %v, 期望 30s", p.Input, p.Start)
		}
		if p.Duration != 20*time.Minute {
			t.Errorf("%s 时长 = %v, 期望 20m", p.Input, p.Duration)
		}
		if p.Output != strings.TrimSuffix(p.Input, ".mkv")+"-trim.mkv" {
			t.Errorf("%s 输出路径 = %q", p.Input, p.Output)
		}
	}
}

// 探测失败的文件也要出现在计划里,并带上跳过原因。
func TestPlansReportsProbeFailure(t *testing.T) {
	dir := t.TempDir()
	p := testkit.WriteFile(t, dir, "a.mkv", "x")
	// Runner 指向不存在的 ffmpeg → 探测必然失败。
	r := &ffmpeg.Runner{Path: filepath.Join(dir, "no-ffmpeg")}

	plans := Plans(context.Background(), r, []scan.Item{{Path: p}}, Options{Head: time.Second}, 1)
	if len(plans) != 1 {
		t.Fatalf("计划数 = %d, 期望 1", len(plans))
	}
	if !plans[0].Skip || plans[0].SkipWhy == "" {
		t.Errorf("探测失败应标记跳过并给出原因: %+v", plans[0])
	}
}

// ffmpeg 执行失败时,Run 必须返回带 ffmpeg 原始输出的错误。
func TestRunCollectsStderr(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRunner(t, "", testkit.ModeFail)
	in := testkit.WriteFile(t, dir, "a.mp4", "x")
	plan := BuildPlan(probeOf(t, in, 600), Options{Head: 10 * time.Second})
	plan.Output = filepath.Join(dir, "out.mp4")

	ex := &Executor{FFmpeg: r, Opts: Options{Head: 10 * time.Second}}
	_, err := ex.Run(context.Background(), plan)
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !strings.Contains(err.Error(), "Invalid argument") {
		t.Errorf("错误应带上 ffmpeg 的 stderr: %v", err)
	}
}

// 跳过的计划不该触发任何 ffmpeg 调用。
func TestRunSkipsSkippedPlan(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRunner(t, "", "")
	in := testkit.WriteFile(t, dir, "a.mp4", "x")
	plan := BuildPlan(probeOf(t, in, 10), Options{Head: 60 * time.Second})
	if !plan.Skip {
		t.Fatal("前置条件:该计划应被跳过")
	}
	ex := &Executor{FFmpeg: r, Opts: Options{Head: 60 * time.Second}}
	out, err := ex.Run(context.Background(), plan)
	if err != nil || out != "" {
		t.Errorf("跳过的计划应返回空输出且不报错,实际 out=%q err=%v", out, err)
	}
}

// 原地替换:成功时替换源文件,失败时不留下半截文件。
func TestRunInPlaceReplacesInput(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRunner(t, "", "")
	in := testkit.WriteFile(t, dir, "a.mp4", strings.Repeat("orig", 8))

	opts := Options{Head: 10 * time.Second, Tail: 10 * time.Second, Suffix: "inplace"}
	plan := BuildPlan(probeOf(t, in, 600), opts)
	if !plan.IsInPlace() {
		t.Fatalf("Suffix=inplace 时输出路径应与输入相同,实际 %q", plan.Output)
	}

	ex := &Executor{FFmpeg: r, Opts: opts}
	out, err := ex.Run(context.Background(), plan)
	if err != nil {
		t.Fatalf("原地替换失败: %v", err)
	}
	if out != in {
		t.Errorf("应返回源文件路径,实际 %q", out)
	}
	st, err := os.Stat(in)
	if err != nil {
		t.Fatalf("源文件丢失: %v", err)
	}
	if st.Size() != 5120 {
		t.Errorf("替换后体积 = %d, 期望 5120", st.Size())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*tmp*")); len(left) != 0 {
		t.Errorf("临时文件应清理: %v", left)
	}
}

// 原地替换遇到 ffmpeg 失败时,临时文件必须被清理、源文件保持不变。
func TestRunInPlaceFailureKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRunner(t, "", testkit.ModeFail)
	in := testkit.WriteFile(t, dir, "a.mp4", "keep-me")

	opts := Options{Head: 10 * time.Second, Suffix: "inplace"}
	plan := BuildPlan(probeOf(t, in, 600), opts)
	ex := &Executor{FFmpeg: r, Opts: opts}
	if _, err := ex.Run(context.Background(), plan); err == nil {
		t.Fatal("ffmpeg 失败应返回错误")
	}
	if got := testkit.ReadFile(t, in); got != "keep-me" {
		t.Errorf("源文件被改动了: %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*tmp*")); len(left) != 0 {
		t.Errorf("失败后不应残留临时文件: %v", left)
	}
}

// Verbose 模式把 ffmpeg 输出直接接到 os.Stderr,行为应与静默模式一致。
func TestRunVerbose(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRunner(t, "", "")
	in := testkit.WriteFile(t, dir, "a.mp4", "x")
	plan := BuildPlan(probeOf(t, in, 600), Options{Head: 10 * time.Second})
	plan.Output = filepath.Join(dir, "out.mp4")

	ex := &Executor{FFmpeg: r, Opts: Options{Head: 10 * time.Second, Verbose: true}}
	if _, err := ex.Run(context.Background(), plan); err != nil {
		t.Fatalf("verbose 模式执行失败: %v", err)
	}
}

// 探测阶段就失败的文件应计入 Failed,并保证汇总里能看到原因。
func TestRunBatchProbeFailure(t *testing.T) {
	dir := t.TempDir()
	p := testkit.WriteFile(t, dir, "a.mkv", "x")
	r := &ffmpeg.Runner{Path: filepath.Join(dir, "no-ffmpeg")}

	res := RunBatch(context.Background(), r, []scan.Item{{Path: p, Size: 10}},
		BatchOptions{Trim: Options{Head: time.Second}})
	if res.Failed != 1 || res.Done != 0 {
		t.Errorf("failed/done = %d/%d, 期望 1/0", res.Failed, res.Done)
	}
	if len(res.Results) != 1 || res.Results[0].Err == nil {
		t.Errorf("应保留失败结果: %+v", res.Results)
	}
	if res.TotalIn != 0 {
		t.Errorf("探测失败的输入不应计入体积: %d", res.TotalIn)
	}
}

// 计划被跳过(头尾超过总时长)时计入 Skipped,不调用 ffmpeg。
func TestRunBatchSkippedItems(t *testing.T) {
	dir := t.TempDir()
	var items []scan.Item
	for _, n := range []string{"a.mkv", "b.mkv"} {
		p := testkit.WriteFile(t, dir, n, "x")
		items = append(items, scan.Item{Path: p, Size: 1024})
	}
	r := newFakeRunner(t, probeStderrOf(items[0].Path), "")

	// 20 分钟的样本,头尾各砍 25 分钟 → 全部跳过。
	res := RunBatch(context.Background(), r, items, BatchOptions{
		Trim:    Options{Head: 25 * time.Minute, Tail: 25 * time.Minute, MinDuration: time.Second},
		Workers: 1,
	})
	if res.Skipped != 2 {
		t.Errorf("skipped = %d, 期望 2", res.Skipped)
	}
	if res.Done != 0 || res.Failed != 0 {
		t.Errorf("done/failed = %d/%d, 期望 0/0", res.Done, res.Failed)
	}
	if res.TotalIn != 2048 {
		t.Errorf("输入体积 = %d, 期望 2048", res.TotalIn)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*-trim.mkv")); len(matches) != 0 {
		t.Errorf("跳过的不应产生输出: %v", matches)
	}
}

// dry-run 只出计划,不落盘。
func TestRunBatchDryRun(t *testing.T) {
	dir := t.TempDir()
	var items []scan.Item
	for _, n := range []string{"a.mkv", "b.mkv"} {
		p := testkit.WriteFile(t, dir, n, "x")
		items = append(items, scan.Item{Path: p, Size: 1024})
	}
	r := newFakeRunner(t, probeStderrOf(items[0].Path), "")

	res := RunBatch(context.Background(), r, items, BatchOptions{
		Trim:    Options{Head: 10 * time.Second, Tail: 10 * time.Second, MinDuration: time.Second, DryRun: true},
		Workers: 2,
	})
	if len(res.Results) != 2 {
		t.Fatalf("结果数 = %d, 期望 2", len(res.Results))
	}
	if res.Done != 0 || res.TotalOut != 0 {
		t.Errorf("dry-run 不应产生实际输出: done=%d out=%d", res.Done, res.TotalOut)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*-trim.mkv")); len(matches) != 0 {
		t.Errorf("dry-run 不应落盘: %v", matches)
	}
}

// 同一个目标被重复请求时,未开 overwrite 的那次应失败。
func TestRunBatchRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	p := testkit.WriteFile(t, dir, "a.mkv", "x")
	r := newFakeRunner(t, probeStderrOf(p), "")
	items := []scan.Item{{Path: p, Size: 1024}}
	base := BatchOptions{
		Trim:    Options{Head: 10 * time.Second, Tail: 10 * time.Second, MinDuration: time.Second, Overwrite: true},
		Workers: 1,
	}

	if res := RunBatch(context.Background(), r, items, base); res.Done != 1 {
		t.Fatalf("首次应成功: %+v", res)
	}
	noOverwrite := base
	noOverwrite.Trim.Overwrite = false
	res := RunBatch(context.Background(), r, items, noOverwrite)
	if res.Failed != 1 {
		t.Errorf("重复执行应失败: %+v", res)
	}
	if res.TotalOut != 0 {
		t.Errorf("失败的输出不应计入体积: %d", res.TotalOut)
	}
}

func TestStatSizeMissingFile(t *testing.T) {
	if _, err := statSize(filepath.Join(t.TempDir(), "ghost")); err == nil {
		t.Error("不存在的文件应报错")
	}
}

// 并发探测先先后后,但计划表的顺序必须跟着入参走:
// 否则同一个命令跑两次,dry-run 表格里的行会换来换去,没法 diff。
func TestPlansKeepsInputOrder(t *testing.T) {
	dir := t.TempDir()
	names := []string{"e01.mkv", "e02.mkv", "e03.mkv", "e04.mkv", "e05.mkv",
		"e06.mkv", "e07.mkv", "e08.mkv", "e09.mkv", "e10.mkv", "e11.mkv", "e12.mkv"}
	var items []scan.Item
	for _, n := range names {
		items = append(items, scan.Item{Path: testkit.WriteFile(t, dir, n, "x"), Size: 512})
	}
	r := newFakeRunner(t, probeStderrOf(items[0].Path), "")

	for round := 0; round < 5; round++ {
		plans := Plans(context.Background(), r, items, Options{Head: 30 * time.Second, Tail: 40 * time.Second}, 4)
		if len(plans) != len(items) {
			t.Fatalf("第 %d 轮: 计划数 = %d, 期望 %d", round, len(plans), len(items))
		}
		for i, p := range plans {
			if p.Input != items[i].Path {
				t.Fatalf("第 %d 轮第 %d 项 = %q, 期望按入参顺序 %q", round, i, p.Input, items[i].Path)
			}
		}
	}
}

// 真实执行时汇总表的顺序同样要稳定。
func TestRunBatchResultsKeepInputOrder(t *testing.T) {
	dir := t.TempDir()
	var items []scan.Item
	for _, n := range []string{"e01.mkv", "e02.mkv", "e03.mkv", "e04.mkv",
		"e05.mkv", "e06.mkv", "e07.mkv", "e08.mkv"} {
		items = append(items, scan.Item{Path: testkit.WriteFile(t, dir, n, "x"), Size: 1024})
	}
	r := newFakeRunner(t, probeStderrOf(items[0].Path), testkit.ModeCopy)

	res := RunBatch(context.Background(), r, items, BatchOptions{
		Trim: Options{
			Head: 10 * time.Second, Tail: 10 * time.Second,
			MinDuration: time.Second, Overwrite: true,
		},
		Workers: 4,
	})
	if len(res.Results) != len(items) {
		t.Fatalf("结果数 = %d, 期望 %d", len(res.Results), len(items))
	}
	for i, got := range res.Results {
		if got.Plan.Input != items[i].Path {
			t.Fatalf("第 %d 项 = %q, 期望按入参顺序 %q", i, got.Plan.Input, items[i].Path)
		}
	}
}
