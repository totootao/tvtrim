package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/totootao/tvtrim/internal/ffmpeg"
	"github.com/totootao/tvtrim/internal/scan"
	"github.com/totootao/tvtrim/internal/testkit"
	"github.com/totootao/tvtrim/internal/trim"
)

// captureOutput 在 fn 执行期间接管标准输出/错误,返回捕获到的文本。
// run() 会打印大量表格,捕获后既能做断言也能保持测试输出干净。
func captureOutput(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道失败: %v", err)
	}
	os.Stdout, os.Stderr = w, w

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	runErr := fn()

	os.Stdout, os.Stderr = oldOut, oldErr
	w.Close()
	out := <-done
	r.Close()
	return out, runErr
}

// fixture 准备好"假 ffmpeg + 两集真实文件"的环境。
func fixture(t *testing.T, files ...string) (string, string) {
	t.Helper()
	root := t.TempDir()
	ff := testkit.WriteFakeFFmpeg(t, root)
	testkit.SetEnv(t, testkit.EnvOut, testkit.WriteFile(t, root, "stderr.txt", combinedStderr))
	dir := filepath.Join(root, "S01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range files {
		testkit.WriteFile(t, dir, n, "fake-media")
	}
	return ff, dir
}

func TestRunPrintsVersion(t *testing.T) {
	out, err := captureOutput(t, func() error { return run([]string{"-version"}) })
	if err != nil {
		t.Fatalf("run(-version) 报错: %v", err)
	}
	if !strings.Contains(out, "tvtrim "+version) {
		t.Errorf("版本输出不符: %q", out)
	}
}

func TestRunRequiresInput(t *testing.T) {
	_, err := captureOutput(t, func() error { return run(nil) })
	if err == nil || !strings.Contains(err.Error(), "请至少指定一个文件或目录") {
		t.Errorf("缺少输入路径时应报错,实际: %v", err)
	}
}

func TestRunArgumentValidation(t *testing.T) {
	ff, dir := fixture(t, "S01E01.mkv")
	file := filepath.Join(dir, "S01E01.mkv")

	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"非法 head", []string{"-head", "abc", file}, "-head 参数无效"},
		{"非法 tail", []string{"-head", "10", "-tail", "xyz", file}, "-tail 参数无效"},
		{"非法 keep", []string{"-head", "10", "-keep", "!", file}, "-keep 参数无效"},
		{"非法 min", []string{"-head", "10", "-min", "?", file}, "-min 参数无效"},
		{"auto 与手动时长冲突", []string{"-auto", "-head", "90", file}, "-auto 与 -head/-tail 不能同时使用"},
		{"web 下 auto 与手动时长冲突", []string{"-web", "-auto", "-head", "90", file}, "-web 模式下请二选一"},
		{"缺少切点", []string{file}, "请至少指定 -head 或 -tail"},
		{"web 缺少切点", []string{"-web", file}, "-web 需要配合 -auto 或 -head/-tail"},
		{"-o 与 -inplace 冲突", []string{"-head", "10", "-o", "x.mkv", "-inplace", file}, "-o 与 -inplace 不能同时使用"},
		{"-o 多文件", []string{"-head", "10", "-o", "x.mkv", file, ff}, "-o 只能在处理单个文件时使用"},
		{"ffmpeg 不可用", []string{"-head", "10", "-ffmpeg", filepath.Join(dir, "nope"), file}, "--ffmpeg 指定的文件不可用"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := captureOutput(t, func() error { return run(c.args) })
			if err == nil {
				t.Fatalf("应报错,却返回 nil")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("错误信息应包含 %q,实际 %q", c.wantErr, err.Error())
			}
		})
	}
}

func TestRunNoMediaFiles(t *testing.T) {
	_, dir := fixture(t) // 空目录
	_, err := captureOutput(t, func() error {
		return run([]string{"-head", "30", "-tail", "40", dir})
	})
	if err == nil || !strings.Contains(err.Error(), "没有找到可处理的媒体文件") {
		t.Fatalf("空目录应报错,实际: %v", err)
	}
}

func TestRunDryRunShowsPlansAndChangesNothing(t *testing.T) {
	ff, dir := fixture(t, "S01E01.mkv", "S01E02.mkv")
	out, err := captureOutput(t, func() error {
		return run([]string{"-head", "30", "-tail", "40", "-dry-run", "-ffmpeg", ff, dir})
	})
	if err != nil {
		t.Fatalf("dry-run 报错: %v", err)
	}
	if !strings.Contains(out, "dry-run") {
		t.Error("应提示这是预览: " + out)
	}
	if !strings.Contains(out, "将被裁剪") {
		t.Error("应打印计划汇总: " + out)
	}
	// 20 分钟样本 -30s -40s = 1130s 输出。
	if !strings.Contains(out, "18m50.0s") {
		t.Errorf("应显示裁剪后时长 1130s(18m50.0s): %s", out)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*-trim.mkv"))
	if len(matches) != 0 {
		t.Errorf("dry-run 不应产生输出文件: %v", matches)
	}
}

func TestRunTrimsFilesInBatch(t *testing.T) {
	ff, dir := fixture(t, "S01E01.mkv", "S01E02.mkv")
	out, err := captureOutput(t, func() error {
		return run([]string{"-head", "30", "-tail", "40", "-j", "2", "-ffmpeg", ff, dir})
	})
	if err != nil {
		t.Fatalf("批量裁剪报错: %v\n%s", err, out)
	}
	for _, n := range []string{"S01E01-trim.mkv", "S01E02-trim.mkv"} {
		st, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			t.Errorf("输出文件缺失: %v", err)
			continue
		}
		if st.Size() < 1024 {
			t.Errorf("%s 体积异常: %d", n, st.Size())
		}
	}
	if !strings.Contains(out, "2 成功") {
		t.Errorf("汇总应显示 2 成功: %s", out)
	}
}

func TestRunAutoDetectAndTrim(t *testing.T) {
	ff, dir := fixture(t, "S01E01.mkv", "S01E02.mkv")
	out, err := captureOutput(t, func() error {
		return run([]string{"-auto", "-ffmpeg", ff, dir})
	})
	if err != nil {
		t.Fatalf("自动识别裁剪报错: %v\n%s", err, out)
	}
	// 样本落在 typical 结构:head=96.5s tail=100s。
	if !strings.Contains(out, "96.5s") || !strings.Contains(out, "100.0s") {
		t.Errorf("应显示自动识别出的切点: %s", out)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*-trim.mkv"))
	if len(matches) != 2 {
		t.Errorf("应产出 2 个文件,实际 %v", matches)
	}
}

func TestRunAutoDryRun(t *testing.T) {
	ff, dir := fixture(t, "S01E01.mkv")
	out, err := captureOutput(t, func() error {
		return run([]string{"-auto", "-dry-run", "-ffmpeg", ff, dir})
	})
	if err != nil {
		t.Fatalf("auto + dry-run 报错: %v\n%s", err, out)
	}
	if !strings.Contains(out, "自动识别结果") {
		t.Errorf("应打印识别结果表: %s", out)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*-trim.mkv"))
	if len(matches) != 0 {
		t.Errorf("dry-run 不应产生文件: %v", matches)
	}
}

func TestRunInplaceReplacesSource(t *testing.T) {
	ff, dir := fixture(t, "S01E01.mkv")
	src := filepath.Join(dir, "S01E01.mkv")
	out, err := captureOutput(t, func() error {
		return run([]string{"-head", "30", "-tail", "40", "-inplace", "-ffmpeg", ff, dir})
	})
	if err != nil {
		t.Fatalf("原地替换报错: %v\n%s", err, out)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*-trim.mkv"))
	if len(matches) != 0 {
		t.Errorf("原地模式不应生成 -trim 文件: %v", matches)
	}
	st, err := os.Stat(src)
	if err != nil {
		t.Fatalf("原文件丢失: %v", err)
	}
	if st.Size() != 5120 {
		t.Errorf("原文件应被替换成裁剪结果(5120 字节),实际 %d", st.Size())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*tmp*")); len(left) != 0 {
		t.Errorf("临时文件未清理: %v", left)
	}
}

func TestRunReportsFailures(t *testing.T) {
	ff, dir := fixture(t, "S01E01.mkv")
	// 让假 ffmpeg 在执行阶段报错。
	testkit.SetEnv(t, testkit.EnvMode, testkit.ModeFail)

	out, err := captureOutput(t, func() error {
		return run([]string{"-head", "30", "-tail", "40", "-ffmpeg", ff, dir})
	})
	if err == nil || !strings.Contains(err.Error(), "处理失败") {
		t.Fatalf("执行失败时应返回错误,实际 err=%v\n%s", err, out)
	}
	if !strings.Contains(out, "失败明细") {
		t.Errorf("应打印失败明细: %s", out)
	}
}

func TestRunEmptyOutputDetected(t *testing.T) {
	ff, dir := fixture(t, "S01E01.mkv")
	testkit.SetEnv(t, testkit.EnvMode, testkit.ModeEmpty)

	out, err := captureOutput(t, func() error {
		return run([]string{"-head", "30", "-tail", "40", "-ffmpeg", ff, dir})
	})
	if err == nil {
		t.Fatalf("输出为空时应报错\n%s", out)
	}
	if !strings.Contains(out, "疑似空文件") {
		t.Errorf("应提示空输出: %s", out)
	}
}

func TestDescribeOutput(t *testing.T) {
	cases := []struct {
		o    cliOptions
		want string
	}{
		{cliOptions{inplace: true}, "原地替换原文件"},
		{cliOptions{output: "/tmp/a.mkv"}, "输出到 /tmp/a.mkv"},
		{cliOptions{suffix: "-cut"}, `同目录生成,后缀 "-cut"`},
	}
	for _, c := range cases {
		got := describeOutput(c.o)
		if got != c.want && !strings.HasPrefix(got, c.want) {
			t.Errorf("describeOutput(%+v) = %q, 期望 %q", c.o, got, c.want)
		}
	}
}

func TestShortName(t *testing.T) {
	long := strings.Repeat("剧", 60)
	got := shortName(long, 10)
	if len([]rune(got)) != 10 {
		t.Errorf("短名长度 = %d, 期望 10", len([]rune(got)))
	}
	if !strings.HasPrefix(got, "…") {
		t.Errorf("长名应以省略号开头: %q", got)
	}
	if !strings.HasSuffix(got, "剧") {
		t.Errorf("应保留尾部: %q", got)
	}
	if got := shortName("短.mkv", 10); got != "短.mkv" {
		t.Errorf("未超长应原样返回,实际 %q", got)
	}
}

func TestPrintPlansCounts(t *testing.T) {
	plans := []*trim.Plan{
		{Input: "/a.mkv", Skip: true, SkipWhy: "时长不足"},
		{Input: "/b.mkv", Duration: 1200 * time.Second, Start: 30 * time.Second,
			End: 1160 * time.Second, OutLength: 1130 * time.Second},
	}
	out, _ := captureOutput(t, func() error {
		willRun, willSkip := printPlans(plans)
		if willRun != 1 || willSkip != 1 {
			t.Errorf("willRun/willSkip = %d/%d, 期望 1/1", willRun, willSkip)
		}
		return nil
	})
	if !strings.Contains(out, "时长不足") || !strings.Contains(out, "1 个将被裁剪, 1 个跳过") {
		t.Errorf("计划表输出不符: %s", out)
	}
}

func TestPrintBatchSummary(t *testing.T) {
	batch := &trim.BatchResult{
		Done: 2, Skipped: 1, Failed: 1, Elapsed: 3 * time.Second,
		TotalIn: 4 << 20, TotalOut: 3 << 20,
		Results: []trim.Result{
			{Plan: &trim.Plan{Input: "/bad.mkv"}, Err: errors.New("炸了")},
			{Plan: &trim.Plan{Input: "/skip.mkv", Skip: true, SkipWhy: "片头已达标"}},
			{Plan: &trim.Plan{Input: "/ok.mkv"}},
		},
	}
	out, _ := captureOutput(t, func() error {
		printBatchSummary(batch, "-trim", false)
		return nil
	})
	for _, want := range []string{"2 成功", "1 跳过", "1 失败", "炸了", "片头已达标", "4.00 MB", "-trim"} {
		if !strings.Contains(out, want) {
			t.Errorf("汇总缺少 %q: %s", want, out)
		}
	}
	// inplace 模式不提示"可删除原文件"。
	out, _ = captureOutput(t, func() error {
		printBatchSummary(batch, "-trim", true)
		return nil
	})
	if strings.Contains(out, "确认无误后可自行删除原文件") {
		t.Error("inplace 模式不应提示删除原文件")
	}
}

func TestRunDryRunViaExplicitFile(t *testing.T) {
	ff, dir := fixture(t, "S01E01.mkv")
	out, err := captureOutput(t, func() error {
		return run([]string{"-head", "30", "-tail", "40", "-dry-run", "-ffmpeg", ff,
			filepath.Join(dir, "S01E01.mkv")})
	})
	if err != nil {
		t.Fatalf("单文件 dry-run 报错: %v\n%s", err, out)
	}
	if !strings.Contains(out, "S01E01.mkv") {
		t.Errorf("应列出该文件: %s", out)
	}
}

// tryOpenBrowser 失败必须静默(测试环境里通常没有 xdg-open)。
func TestTryOpenBrowserIsSilent(t *testing.T) {
	tryOpenBrowser("http://127.0.0.1:1/")
}

// 扫描出的 Item 与 Web 视图共用一套时长换算,这里锁住 fmtDur 的输出。
func TestFmtDurAgainstProbeDuration(t *testing.T) {
	p := &ffmpeg.ProbeResult{Duration: 1130 * time.Second}
	it := scan.Item{Path: "/x.mkv"}
	if it.Path == "" || p.DurationSeconds() != 1130 {
		t.Fatal("前置条件不成立")
	}
	if got := fmtDur(p.Duration); got != "18m50.0s" {
		t.Errorf("fmtDur(1130s) = %q, 期望 18m50.0s", got)
	}
	if got := fmtDur(0); got != "0s" {
		t.Errorf("fmtDur(0) = %q, 期望 0s", got)
	}
	if got := fmtDur(-90 * time.Second); got != "-1m30.0s" {
		t.Errorf("fmtDur(-90s) = %q, 期望 -1m30.0s", got)
	}
}
