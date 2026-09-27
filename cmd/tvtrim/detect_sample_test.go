package main

import (
	"strings"
	"testing"

	"github.com/totootao/tvtrim/internal/auto"
	"github.com/totootao/tvtrim/internal/scan"
)

// itemsIn 构造一组 Item,文件名形如 S01E01.mkv 之类。
func itemsIn(dir string, names ...string) []scan.Item {
	var items []scan.Item
	for _, n := range names {
		it := scan.Item{Path: dir + "/" + n}
		it.Show = scan.ShowOfPath(it.Path)
		it.Season, it.Episode = scan.ParseEpisode(n)
		items = append(items, it)
	}
	return items
}

func TestPickSilenceTargetsAllWhenDisabled(t *testing.T) {
	items := itemsIn("/tv/秀", "S01E01.mkv", "S01E02.mkv", "S01E03.mkv")
	got := pickSilenceTargets(items, 0)
	if len(got) != 3 {
		t.Errorf("sample=0 时应全部选中, 实际 %d", len(got))
	}
}

// TestPickSilenceTargetsPerShow 验证每部剧各自抽样,
// 不会因为剧 A 排在前面就把名额占满。
func TestPickSilenceTargetsPerShow(t *testing.T) {
	items := append(
		itemsIn("/tv/秀A", "S01E01.mkv", "S01E02.mkv", "S01E03.mkv", "S01E04.mkv"),
		itemsIn("/tv/秀B", "S01E01.mkv", "S01E02.mkv")...,
	)
	got := pickSilenceTargets(items, 2)
	// 秀A 取前 2 集,秀B 只有 2 集全取 → 共 4 个。
	if len(got) != 4 {
		t.Fatalf("选中 %d 个, 期望 4", len(got))
	}
	for _, want := range []string{"/tv/秀A/S01E01.mkv", "/tv/秀A/S01E02.mkv",
		"/tv/秀B/S01E01.mkv", "/tv/秀B/S01E02.mkv"} {
		if !got[want] {
			t.Errorf("%s 应被选中", want)
		}
	}
	for _, bad := range []string{"/tv/秀A/S01E03.mkv", "/tv/秀A/S01E04.mkv"} {
		if got[bad] {
			t.Errorf("%s 不该被选中", bad)
		}
	}
}

// TestPickSilenceTargetsPicksEarliest 验证取的是集号最小的几集。
func TestPickSilenceTargetsPicksEarliest(t *testing.T) {
	items := itemsIn("/tv/秀", "S01E05.mkv", "S01E01.mkv", "S01E03.mkv")
	got := pickSilenceTargets(items, 1)
	for _, want := range []string{"/tv/秀/S01E01.mkv"} {
		if !got[want] {
			t.Errorf("%s 应被选中", want)
		}
	}
	if len(got) != 1 {
		t.Errorf("选中 %d 个, 期望 1", len(got))
	}
}

func TestGroupByShowSortsAndGroups(t *testing.T) {
	items := append(
		itemsIn("/tv/秀A", "S01E03.mkv", "S01E01.mkv", "S01E02.mkv"),
		itemsIn("/tv/秀B", "S01E01.mkv")...,
	)
	groups := groupByShow(items)
	if len(groups) != 2 {
		t.Fatalf("分组数 = %d, 期望 2", len(groups))
	}
	a := groups[0].items
	if len(a) != 3 {
		t.Fatalf("秀A 数量 = %d", len(a))
	}
	want := []string{"S01E01.mkv", "S01E02.mkv", "S01E03.mkv"}
	for i, n := range want {
		if got := a[i].Path; !strings.HasSuffix(got, n) {
			t.Errorf("第 %d 项 = %s, 期望以 %s 结尾", i, got, n)
		}
	}
}

// TestLessEpisodeUnnumberedLast 无集号的文件应排在组尾,不做抽样样本。
func TestLessEpisodeUnnumberedLast(t *testing.T) {
	items := itemsIn("/tv/秀", "花絮.mkv", "S01E02.mkv", "S01E01.mkv")
	groups := groupByShow(items)
	last := groups[0].items[len(groups[0].items)-1]
	if !strings.HasSuffix(last.Path, "花絮.mkv") {
		t.Errorf("无集号文件应排最后, 实际最后是 %s", last.Path)
	}
}

func TestHasConsensus(t *testing.T) {
	items := itemsIn("/tv/秀", "S01E01.mkv", "S01E02.mkv", "S01E03.mkv")
	mk := func(head float64) auto.Result {
		return auto.Result{Head: head, Tail: 45, OK: true}
	}
	cases := []struct {
		name string
		res  map[string]auto.Result
		want bool
	}{
		{"两集一致", map[string]auto.Result{
			"/tv/秀/S01E01.mkv": mk(30), "/tv/秀/S01E02.mkv": mk(30)}, true},
		{"三集互不相同", map[string]auto.Result{
			"/tv/秀/S01E01.mkv": mk(30), "/tv/秀/S01E02.mkv": mk(60),
			"/tv/秀/S01E03.mkv": mk(90)}, false},
		{"只有一集成功", map[string]auto.Result{"/tv/秀/S01E01.mkv": mk(30)}, false},
		{"没有结果", map[string]auto.Result{}, false},
	}
	for _, c := range cases {
		if got := hasConsensus(c.res, items); got != c.want {
			t.Errorf("%s: hasConsensus = %v, 期望 %v", c.name, got, c.want)
		}
	}
}

func TestExpandInconsistentGroups(t *testing.T) {
	items := append(
		itemsIn("/tv/秀A", "S01E01.mkv", "S01E02.mkv", "S01E03.mkv"),
		itemsIn("/tv/秀B", "S01E01.mkv", "S01E02.mkv")...,
	)
	mk := func(head float64) auto.Result { return auto.Result{Head: head, Tail: 45, OK: true} }
	// 秀A 三集互不相同(无共识) → 整组重测;秀B 两集一致 → 不重测。
	res := map[string]auto.Result{
		"/tv/秀A/S01E01.mkv": mk(30), "/tv/秀A/S01E02.mkv": mk(60), "/tv/秀A/S01E03.mkv": mk(90),
		"/tv/秀B/S01E01.mkv": mk(30), "/tv/秀B/S01E02.mkv": mk(30),
	}
	extra := expandInconsistentGroups(items, res, 2)
	if len(extra) != 3 {
		t.Fatalf("追加项 = %d, 期望 3(秀A 全部)", len(extra))
	}
	for p := range extra {
		if !strings.Contains(p, "秀A") {
			t.Errorf("不该把已达成共识的剧纳入重测: %s", p)
		}
	}
	// sample=0 表示从未抽样,不存在"抽样不一致"的问题。
	if got := expandInconsistentGroups(items, res, 0); got != nil {
		t.Errorf("sample=0 时应返回 nil, 实际 %v", got)
	}
}

// TestBroadcastShowValues 验证抽样结论会被套用到同剧未抽样的集,
// 且不会乐观覆盖"抽了样却识别失败"的文件。
func TestBroadcastShowValues(t *testing.T) {
	items := itemsIn("/tv/秀", "S01E01.mkv", "S01E02.mkv", "S01E03.mkv", "S01E04.mkv")
	ordered := []detectResult{
		{Item: items[0], Res: auto.Result{Head: 30, Tail: 45, OK: true, Note: "片头切点 30.0s"}},
		{Item: items[1], Res: auto.Result{Head: 30, Tail: 45, OK: true, Note: "片头切点 30.0s"}},
		{Item: items[2], Res: auto.Result{Head: 30, Tail: 45, OK: true, Note: "片头切点 30.0s"}},
		// E04 未抽样:没有 Err,但 Res 未成功。
		{Item: items[3]},
	}
	broadcastShowValues(ordered)
	got := ordered[3].Res
	if got.Head != 30 || got.Tail != 45 || !got.OK {
		t.Fatalf("未抽样集应沿用本剧结论, 实际 %+v", got)
	}
	if !strings.Contains(got.Note, "沿用本剧") {
		t.Errorf("Note 应说明来源, 实际 %q", got.Note)
	}
}

// TestBroadcastShowValuesSkipsFailures 抽样了却失败的文件不该被套用。
func TestBroadcastShowValuesSkipsFailures(t *testing.T) {
	items := itemsIn("/tv/秀", "S01E01.mkv", "S01E02.mkv", "S01E03.mkv")
	ordered := []detectResult{
		{Item: items[0], Res: auto.Result{Head: 30, Tail: 45, OK: true, Note: "x"}},
		{Item: items[1], Res: auto.Result{Head: 30, Tail: 45, OK: true, Note: "x"}},
		{Item: items[2], Err: errStub},
	}
	broadcastShowValues(ordered)
	if ordered[2].Res.OK {
		t.Error("识别失败的文件不该被套用本剧结论")
	}
}

var errStub = &broadcastStub{}

type broadcastStub struct{}

func (b *broadcastStub) Error() string { return "stub" }

// TestRefreshAgreementNotesCoverAllEpisodes 推广之后一致率的分母
// 应该是全集数,而不是抽样数。
func TestRefreshAgreementNotesCoverAllEpisodes(t *testing.T) {
	items := itemsIn("/tv/秀", "S01E01.mkv", "S01E02.mkv", "S01E03.mkv", "S01E04.mkv")
	ordered := make([]detectResult, 4)
	for i := range ordered {
		ordered[i] = detectResult{Item: items[i],
			Res: auto.Result{Head: 30, Tail: 45, OK: true, Note: "[一致 3/3] 片头切点 30.0s"}}
	}
	refreshAgreementNotes(ordered)
	for i, d := range ordered {
		if !strings.HasPrefix(d.Res.Note, "[一致 4/4] ") {
			t.Errorf("第 %d 集前缀 = %q, 期望 [一致 4/4] ", i, d.Res.Note)
		}
		if strings.Contains(d.Res.Note, "[一致 4/4] [一致") {
			t.Errorf("第 %d 集前缀重复: %q", i, d.Res.Note)
		}
	}
}

// TestExpandInconsistentGroupsSingleSample 明确只要 1 集时不再要求多数派,
// 否则 -sample 1 会永远退化成全量重测。
func TestExpandInconsistentGroupsSingleSample(t *testing.T) {
	items := itemsIn("/tv/秀", "S01E01.mkv", "S01E02.mkv", "S01E03.mkv")
	mk := func(head float64) auto.Result { return auto.Result{Head: head, Tail: 45, OK: true} }
	res := map[string]auto.Result{
		"/tv/秀/S01E01.mkv": mk(30), "/tv/秀/S01E02.mkv": mk(60), "/tv/秀/S01E03.mkv": mk(90),
	}
	for _, sample := range []int{0, 1, -1} {
		if got := expandInconsistentGroups(items, res, sample); got != nil {
			t.Errorf("sample=%d 时不该触发重测, 实际 %v", sample, got)
		}
	}
}

func TestFprintlnNilWriter(t *testing.T) {
	fprintln(nil, "这行不会写出去") // 只需不 panic
}
