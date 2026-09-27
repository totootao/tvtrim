package trim

import (
	"slices"
	"testing"
	"time"

	"github.com/totootao/tvtrim/internal/ffmpeg"
)

// mkProbe 构造一个用于测试的探测结果。
func mkProbe(path string, secs float64) *ffmpeg.ProbeResult {
	return &ffmpeg.ProbeResult{
		Path:     path,
		Duration: time.Duration(secs * float64(time.Second)),
		HasVideo: true,
		HasAudio: true,
	}
}

func TestBuildPlanBasic(t *testing.T) {
	// 45 分钟的剧集,砍掉 90 秒片头与 60 秒片尾。
	p := mkProbe("/tv/ep01.mp4", 45*60)
	plan := BuildPlan(p, Options{Head: 90 * time.Second, Tail: 60 * time.Second})

	if plan.Skip {
		t.Fatalf("不应跳过: %s", plan.SkipWhy)
	}
	if plan.Start != 90*time.Second {
		t.Errorf("起点应为 90s,实际 %v", plan.Start)
	}
	if plan.End != 44*60*time.Second {
		t.Errorf("终点应为 44m0s(2700-60),实际 %v", plan.End)
	}
	if got := plan.OutLength.Seconds(); got != 2550 {
		t.Errorf("裁剪后应为 2550s(2700-90-60),实际 %v", got)
	}
}

func TestBuildPlanWithKeep(t *testing.T) {
	// Keep 让结尾多砍一点,即输出更短。
	p := mkProbe("/tv/ep01.mp4", 600)
	plan := BuildPlan(p, Options{
		Head: 60 * time.Second,
		Tail: 30 * time.Second,
		Keep: 10 * time.Second,
	})
	if plan.End != 560*time.Second {
		t.Errorf("终点应为 560s,实际 %v", plan.End)
	}
}

func TestBuildPlanSkipWhenTooShort(t *testing.T) {
	cases := []struct {
		name string
		dur  float64
		opts Options
	}{
		{
			name: "头尾超过总时长",
			dur:  100,
			opts: Options{Head: 80 * time.Second, Tail: 40 * time.Second},
		},
		{
			name: "裁剪后低于下限",
			dur:  100,
			opts: Options{Head: 50 * time.Second, Tail: 45 * time.Second},
		},
		{
			name: "未指定头尾",
			dur:  100,
			opts: Options{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := BuildPlan(mkProbe("/tv/x.mp4", c.dur), c.opts)
			if !plan.Skip {
				t.Errorf("应被跳过,但实际 OutLength=%v", plan.OutLength)
			}
			if plan.SkipWhy == "" {
				t.Error("跳过时应给出原因")
			}
		})
	}
}

func TestBuildPlanClampsTail(t *testing.T) {
	// 片尾时长超过总时长时,终点应钳制到 0 而不是负数。
	p := mkProbe("/tv/x.mp4", 100)
	plan := BuildPlan(p, Options{Head: 10 * time.Second, Tail: 500 * time.Second})
	if plan.End < 0 {
		t.Errorf("终点不应为负: %v", plan.End)
	}
	if !plan.Skip {
		t.Error("这种情况应被跳过")
	}
}

func TestBuildPlanCustomMinDuration(t *testing.T) {
	// 把下限调低后,一个原本会被跳过的短片段应能通过。
	p := mkProbe("/tv/clip.mp4", 30)
	opts := Options{Head: 10 * time.Second, Tail: 10 * time.Second}

	if plan := BuildPlan(p, opts); plan.Skip {
		t.Errorf("默认下限 10s 下 10s 输出应通过,实际跳过: %s", plan.SkipWhy)
	}

	opts.MinDuration = 15 * time.Second
	if plan := BuildPlan(p, opts); !plan.Skip {
		t.Error("下限设为 15s 后 10s 输出应被跳过")
	}
}

func TestOutputPath(t *testing.T) {
	cases := []struct {
		name  string
		input string
		opts  Options
		want  string
	}{
		{"默认后缀", "/tv/ep01.mp4", Options{}, "/tv/ep01-trim.mp4"},
		{"自定义后缀", "/tv/ep01.mkv", Options{Suffix: "_clean"}, "/tv/ep01_clean.mkv"},
		{"原地替换", "/tv/ep01.ts", Options{Suffix: "inplace"}, "/tv/ep01.ts"},
		{"显式输出", "/tv/ep01.avi", Options{Output: "/out/final.avi"}, "/out/final.avi"},
		{"中文文件名", "/剧集/第01集.mp4", Options{}, "/剧集/第01集-trim.mp4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := OutputPath(c.input, c.opts); got != c.want {
				t.Errorf("期望 %q,实际 %q", c.want, got)
			}
		})
	}
}

func TestIsInPlace(t *testing.T) {
	in := BuildPlan(mkProbe("/tv/a.mp4", 100), Options{Head: time.Second, Suffix: "inplace"})
	if !in.IsInPlace() {
		t.Error("suffix=inplace 应判定为原地替换")
	}

	out := BuildPlan(mkProbe("/tv/a.mp4", 100), Options{Head: time.Second})
	if out.IsInPlace() {
		t.Error("默认后缀不应判定为原地替换")
	}
}

func TestWindowSeconds(t *testing.T) {
	p := mkProbe("/tv/a.mp4", 1000)
	plan := BuildPlan(p, Options{Head: 90 * time.Second, Tail: 30 * time.Second})
	start, end := plan.WindowSeconds()
	if start != 90 {
		t.Errorf("start 应为 90,实际 %v", start)
	}
	if end != 970 {
		t.Errorf("end 应为 970,实际 %v", end)
	}
}

func TestMuxerFor(t *testing.T) {
	cases := map[string]string{
		"a.mp4": "mp4",
		"a.m4v": "mp4",
		"a.mov": "mp4",
		"a.mkv": "matroska",
		"a.ts":  "mpegts",
		"a.avi": "avi",
		"a.mp3": "mp3",
		"A.MP4": "mp4",
		"a.xyz": "matroska",
	}
	for in, want := range cases {
		if got := muxerFor(in); got != want {
			t.Errorf("muxerFor(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// TestBuildArgsPlacesSSAfterInput 锁定几个关键约束:
//   - ffmpeg-trim 会静默忽略 `-i` 之前的 `-ss`,所以 -ss 必须放在输入之后。
//   - 用相对时长 -t(终点-起点)而非绝对 -to,裁剪长度与 muxer 无关。
//   - 必须加 -avoid_negative_ts make_zero 与 -fflags +genpts 归零并重排时间戳。
//
// 早期版本把 -ss 放在输入前,导致片头根本没被砍掉;用 -to 则 asf/wmv 会因
// 关键帧对齐多留一个 GOP。这里回归防护。
func TestBuildArgsPlacesSSAfterInput(t *testing.T) {
	exec := &Executor{FFmpeg: &ffmpeg.Runner{Path: "ffmpeg"}}
	plan := BuildPlan(mkProbe("/tv/a.mp4", 1000), Options{Head: 90 * time.Second, Tail: 30 * time.Second})

	args := exec.buildArgs(plan, "/tv/a-trim.mp4")
	idxSS, idxI, idxT, idxCopy := -1, -1, -1, -1
	for i, a := range args {
		switch a {
		case "-ss":
			idxSS = i
		case "-i":
			idxI = i
		case "-t":
			idxT = i
		case "copy":
			if i > 0 && args[i-1] == "-c" {
				idxCopy = i
			}
		}
	}
	if idxSS < 0 || idxI < 0 || idxT < 0 || idxCopy < 0 {
		t.Fatalf("参数缺失: %v", args)
	}
	if idxSS < idxI {
		t.Errorf("-ss 必须在 -i 之后(ffmpeg-trim 会忽略输入侧 -ss): %v", args)
	}
	if idxT < idxI {
		t.Errorf("-t 必须在 -i 之后: %v", args)
	}
	if idxSS > idxT {
		t.Errorf("-ss 应排在 -t 之前: %v", args)
	}
	if got := args[idxSS+1]; got != "90.000" {
		t.Errorf("-ss 值应为 90.000,实际 %q", got)
	}
	// 裁剪时长 = (1000-30) - 90 = 880s
	if got := args[idxT+1]; got != "880.000" {
		t.Errorf("-t 值应为 880.000,实际 %q", got)
	}
	// 时间戳修正 flag 必须存在
	if !slices.Contains(args, "-avoid_negative_ts") || !slices.Contains(args, "make_zero") {
		t.Errorf("缺少 -avoid_negative_ts make_zero: %v", args)
	}
	if !slices.Contains(args, "-fflags") || !slices.Contains(args, "+genpts") {
		t.Errorf("缺少 -fflags +genpts: %v", args)
	}
}

func TestBuildArgsOmitsSSWhenZero(t *testing.T) {
	exec := &Executor{FFmpeg: &ffmpeg.Runner{Path: "ffmpeg"}}
	plan := BuildPlan(mkProbe("/tv/a.mp4", 1000), Options{Tail: 30 * time.Second})
	for _, a := range exec.buildArgs(plan, "/tv/a-trim.mp4") {
		if a == "-ss" {
			t.Error("Head 为 0 时不应出现 -ss")
		}
	}
}

// TestBuildArgsInjectsITSOffset 锁定时间戳补偿的关键约束:
// -itsoffset 是输入侧选项,必须出现在 -i 之前;
// 它用来抵消输出侧 -ss 回溯到关键帧造成的"视频流起点偏移"。
func TestBuildArgsInjectsITSOffset(t *testing.T) {
	exec := &Executor{FFmpeg: &ffmpeg.Runner{Path: "ffmpeg"}}
	plan := BuildPlan(mkProbe("/tv/a.mp4", 1000), Options{Head: 90 * time.Second, Tail: 30 * time.Second})
	plan.TSOffset = 9954 * time.Millisecond

	args := exec.buildArgs(plan, "/tv/a-trim.mp4")
	idxOff, idxI := -1, -1
	for i, a := range args {
		switch a {
		case "-itsoffset":
			idxOff = i
		case "-i":
			idxI = i
		}
	}
	if idxOff < 0 {
		t.Fatalf("TSOffset>0 时必须出现 -itsoffset: %v", args)
	}
	if idxI < 0 {
		t.Fatalf("缺少 -i: %v", args)
	}
	if idxOff > idxI {
		t.Errorf("-itsoffset 必须放在 -i 之前(输入侧选项): %v", args)
	}
	if got := args[idxOff+1]; got != "9.954" {
		t.Errorf("-itsoffset 值应为 9.954,实际 %q", got)
	}
}

// TestBuildArgsNoITSOffsetWhenZero TSOffset 为 0 时不应写入 -itsoffset,
// 保持与旧版本完全一致的命令行(避免影响本来正常的文件)。
func TestBuildArgsNoITSOffsetWhenZero(t *testing.T) {
	exec := &Executor{FFmpeg: &ffmpeg.Runner{Path: "ffmpeg"}}
	plan := BuildPlan(mkProbe("/tv/a.mp4", 1000), Options{Head: 90 * time.Second, Tail: 30 * time.Second})
	if plan.TSOffset != 0 {
		t.Fatalf("新构造的 plan 不应带 TSOffset,实际 %v", plan.TSOffset)
	}
	for _, a := range exec.buildArgs(plan, "/tv/a-trim.mp4") {
		if a == "-itsoffset" {
			t.Errorf("TSOffset=0 时不应出现 -itsoffset: %v", exec.buildArgs(plan, "/tv/a-trim.mp4"))
		}
	}
}
