package auto

import (
	"testing"

	"github.com/totootao/tvtrim/internal/ffmpeg"
)

// mkSil 构造一段静音区间。
func mkSil(start, end float64) ffmpeg.SilenceRange {
	return ffmpeg.SilenceRange{Start: start, End: end, Duration: end - start}
}

// typicalSilences 构造典型剧集结构(总长 1200s):
//
//	[黑屏 0-2] [OP 2-95] [静音 95-96.5] [正片 96.5-1100] [静音 1100-1101.5] [ED 1101.5-1135] [静音 1135-1200]
func typicalSilences() []ffmpeg.SilenceRange {
	return []ffmpeg.SilenceRange{
		mkSil(0, 2),
		mkSil(95, 96.5),
		mkSil(1100, 1101.5),
		mkSil(1135, 1200),
	}
}

func TestDetectTypical(t *testing.T) {
	res, err := Detect(1200, typicalSilences())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if res.Head != 96.5 {
		t.Errorf("Head 期望 96.5(黑屏+OP 结束),得到 %.2f", res.Head)
	}
	// 片尾切在"正片→ED 静音"的 Start = 1100;tail = 1200 - 1100 = 100。
	if res.Tail != 100 {
		t.Errorf("Tail 期望 100,得到 %.2f", res.Tail)
	}
	if !res.OK {
		t.Errorf("应识别成功")
	}
}

func TestDetectHardCutBeforeED(t *testing.T) {
	// 正片与 ED 之间没有静音(硬切):
	// [黑屏 0-2][OP 2-95][静音 95-96.5][正片 96.5-1100(硬切)][ED 1100-1135][静音 1135-1200]
	sils := []ffmpeg.SilenceRange{
		mkSil(0, 2),
		mkSil(95, 96.5),
		mkSil(1135, 1200),
	}
	res, err := Detect(1200, sils)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if res.Head != 96.5 {
		t.Errorf("Head 期望 96.5,得到 %.2f", res.Head)
	}
	// 退化为只剪 ED 之后的静音:tail = 1200 - 1135 = 65。
	if res.Tail != 65 {
		t.Errorf("Tail 期望 65(仅剪 ED 后部分),得到 %.2f", res.Tail)
	}
}

func TestDetectNoTailSilence(t *testing.T) {
	// 文件在正片结束后立即结束,无末尾静音:
	// [黑屏 0-2][OP 2-95][静音 95-96.5][正片 96.5-1200]
	sils := []ffmpeg.SilenceRange{
		mkSil(0, 2),
		mkSil(95, 96.5),
	}
	res, err := Detect(1200, sils)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if res.Head != 96.5 {
		t.Errorf("Head 期望 96.5,得到 %.2f", res.Head)
	}
	if res.Tail != 0 {
		t.Errorf("Tail 期望 0,得到 %.2f", res.Tail)
	}
}

func TestDetectShortSilencesOnly(t *testing.T) {
	// 只有对白级短停顿,不足以构成切点。
	sils := []ffmpeg.SilenceRange{mkSil(100, 100.4), mkSil(1150, 1150.5)}
	if _, err := Detect(1200, sils); err == nil {
		t.Error("短静音不足时应报错")
	}
}

func TestDetectNoSilences(t *testing.T) {
	if _, err := Detect(1200, nil); err == nil {
		t.Error("无静音区间时应报错")
	}
	if _, err := Detect(0, typicalSilences()); err == nil {
		t.Error("总时长未知时应报错")
	}
}

func TestCorrectFixesOutlier(t *testing.T) {
	base := Result{OK: true, Head: 96.5, Tail: 98.5}
	rs := []Result{base, base, base, base, {OK: true, Head: 88.0, Tail: 98.5}}
	out := Correct(rs)

	// 前 4 个不变。
	for i := 0; i < 4; i++ {
		if out[i].Fixed {
			t.Errorf("集 %d 不应被修正", i)
		}
	}
	// 第 5 个片头离群,应修正为众数 96.5。
	if !out[4].Fixed {
		t.Fatal("离群集应被标记 Fixed")
	}
	if out[4].Head != 96.5 {
		t.Errorf("离群片头应修正为 96.5,得到 %.2f", out[4].Head)
	}
}

func TestCorrectKeepsUnanimous(t *testing.T) {
	base := Result{OK: true, Head: 96.5, Tail: 98.5}
	out := Correct([]Result{base, base, base})
	for i, r := range out {
		if r.Fixed {
			t.Errorf("集 %d 不应被修正", i)
		}
	}
}

func TestCorrectSingleResult(t *testing.T) {
	base := Result{OK: true, Head: 96.5, Tail: 98.5}
	out := Correct([]Result{base})
	if out[0].Fixed {
		t.Error("单集不应做纠错")
	}
}

func TestAgreement(t *testing.T) {
	base := Result{OK: true, Head: 96.5, Tail: 98.5}
	odd := Result{OK: true, Head: 88.0, Tail: 98.5}
	rs := []Result{base, base, base, odd}
	if agree, total := Agreement(rs, 0); agree != 3 || total != 4 {
		t.Errorf("期望 3/4,得到 %d/%d", agree, total)
	}
	if agree, _ := Agreement(rs, 3); agree != 1 {
		t.Errorf("离群集自身一致数期望 1,得到 %d", agree)
	}
}

func TestModeOfSpread(t *testing.T) {
	// 值在容差带内浮动(关键帧对齐),应聚成一簇。
	vals := []float64{96.2, 96.5, 96.8, 96.4, 150.0}
	m := modeOf(vals, ModeTolerance)
	if !m.ok {
		t.Fatal("应找到多数派簇")
	}
	if m.value < 96.2 || m.value > 96.8 {
		t.Errorf("众数应落在 96.2~96.8,得到 %.2f", m.value)
	}
}
