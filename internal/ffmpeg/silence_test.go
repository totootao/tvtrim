package ffmpeg

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseSilences(t *testing.T) {
	out := `[silencedetect @ 0x55] silence_start: 1.2
[silencedetect @ 0x55] silence_end: 3.456 | silence_duration: 2.256
[silencedetect @ 0x55] silence_start: 100
[silencedetect @ 0x55] silence_end: 101.5 | silence_duration: 1.5
`
	got := ParseSilences(out, 0)
	if len(got) != 2 {
		t.Fatalf("期望 2 个静音区间,得到 %d 个", len(got))
	}
	if got[0].Start != 1.2 || got[0].End != 3.456 || got[0].Duration != 2.256 {
		t.Errorf("区间 0 不符: %+v", got[0])
	}
	if got[1].Start != 100 || got[1].End != 101.5 {
		t.Errorf("区间 1 不符: %+v", got[1])
	}
}

func TestParseSilencesTrailingOpen(t *testing.T) {
	// 文件以静音收尾:最后的 silence_start 没有对应的 end。
	out := `[silencedetect @ 0x55] silence_start: 5
[silencedetect @ 0x55] silence_end: 6 | silence_duration: 1
[silencedetect @ 0x55] silence_start: 118
`
	got := ParseSilences(out, 120)
	if len(got) != 2 {
		t.Fatalf("期望 2 个静音区间,得到 %d 个", len(got))
	}
	last := got[1]
	if last.Start != 118 || last.End != 120 || last.Duration != 2 {
		t.Errorf("末尾未闭合区间应补齐到总时长: %+v", last)
	}
}

func TestParseSilencesTrailingOpenNoTotal(t *testing.T) {
	// 没给总时长时,未闭合区间应被丢弃而不是产生负时长。
	out := "[silencedetect @ 0x55] silence_start: 118\n"
	if got := ParseSilences(out, 0); len(got) != 0 {
		t.Errorf("totalSec=0 时应丢弃未闭合区间,得到 %+v", got)
	}
}

func TestParseSilencesEmpty(t *testing.T) {
	if got := ParseSilences("", 100); len(got) != 0 {
		t.Errorf("空输入应返回空,得到 %+v", got)
	}
}

// TestDetectSilencesLive 在有 ffmpeg 可用时做一次端到端静音检测。
func TestDetectSilencesLive(t *testing.T) {
	ff, ok := os.LookupEnv("TVTRIM_TEST_FFMPEG")
	if !ok {
		t.Skip("未设置 TVTRIM_TEST_FFMPEG,跳过真机测试")
	}
	if runtime.GOOS == "windows" {
		t.Skip("null muxer 输出重定向在 Windows 下未验证")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "sil.mp4")

	// 生成素材需要 lavfi(ffmpeg-trim 没有),用系统完整版 ffmpeg;
	// 检测目标用 TVTRIM_TEST_FFMPEG(通常是 ffmpeg-trim)—— 与真实场景一致。
	gen, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("系统无 ffmpeg,无法生成测试素材")
	}
	// 5s 音频:0-1s 静音 + 1-4s 正弦 + 4-5s 静音(enable 表达式静音首尾各 1s)。
	cmd := exec.Command(gen, "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=5",
		"-af", `volume=0:enable=lt(t\,1)+gte(t\,4)`,
		"-c:a", "aac", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("生成测试音频失败: %v\n%s", err, out)
	}

	r := &Runner{Path: ff}
	sils, err := r.DetectSilences(context.Background(), path, -35, 0.5, 5)
	if err != nil {
		t.Fatalf("DetectSilences: %v", err)
	}
	if len(sils) != 2 {
		t.Fatalf("期望 2 个静音区间(头尾各 1s),得到 %d: %+v", len(sils), sils)
	}
	if sils[0].Start > 0.3 || sils[0].End < 0.8 {
		t.Errorf("开头静音区间异常: %+v", sils[0])
	}
	if sils[1].Start < 3.7 || sils[1].End < 4.7 {
		t.Errorf("结尾静音区间异常: %+v", sils[1])
	}
}
