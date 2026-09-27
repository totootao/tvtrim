package web

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/totootao/tvtrim/internal/auto"
	"github.com/totootao/tvtrim/internal/ffmpeg"
	"github.com/totootao/tvtrim/internal/scan"
)

func itemOf(dir, name string, size int64, season, ep int) scan.Item {
	return scan.Item{
		Path: filepath.Join(dir, name), Size: size, Season: season, Episode: ep,
	}
}

func probeOf(secs float64) *ffmpeg.ProbeResult {
	return &ffmpeg.ProbeResult{
		Duration:  time.Duration(secs * float64(time.Second)),
		Container: "mov,mp4,m4a,3gp,3g2,mj2",
		HasVideo:  true,
		HasAudio:  true,
	}
}

func TestBuildShowsManualMode(t *testing.T) {
	dirA := filepath.Join("/media", "剧集A")
	dirB := filepath.Join("/media", "剧集B")
	items := []scan.Item{
		itemOf(dirA, "S01E02.mkv", 100, 1, 2),
		itemOf(dirA, "S01E01.mkv", 100, 1, 1),
		itemOf(dirB, "S01E01.mp4", 300, 1, 1),
	}
	probes := map[string]*ffmpeg.ProbeResult{
		filepath.Join(dirA, "S01E01.mkv"): probeOf(1200),
		filepath.Join(dirA, "S01E02.mkv"): probeOf(1200),
		filepath.Join(dirB, "S01E01.mp4"): probeOf(2400),
	}

	shows := BuildShows(items, probes, nil, 30, 45)
	if len(shows) != 2 {
		t.Fatalf("应按目录分成 2 部剧,实际 %d", len(shows))
	}

	// 分组 key 是目录,排序后 /media/剧集A 在前。
	if shows[0].Dir != dirA || shows[1].Dir != dirB {
		t.Fatalf("分组顺序不符: %s, %s", shows[0].Dir, shows[1].Dir)
	}
	if base := filepath.Base(dirA); shows[0].Name != base+"  S01" {
		t.Errorf("剧名应带季号后缀,实际 %q", shows[0].Name)
	}

	a := shows[0]
	if len(a.Items) != 2 {
		t.Fatalf("剧集A 应有 2 集,实际 %d", len(a.Items))
	}
	if a.Items[0].Episode != 1 || a.Items[1].Episode != 2 {
		t.Errorf("集号应按升序排: E%d, E%d", a.Items[0].Episode, a.Items[1].Episode)
	}
	if a.Ready != 2 || a.TotalIn != 200 {
		t.Errorf("Ready=%d TotalIn=%d, 期望 2 / 200", a.Ready, a.TotalIn)
	}
	for _, it := range a.Items {
		if it.Head != 30 || it.Tail != 45 {
			t.Errorf("%s: head/tail = %v/%v, 期望 30/45", it.Name, it.Head, it.Tail)
		}
		if !it.Ready || it.Note != "手动指定" {
			t.Errorf("%s: ready=%v note=%q", it.Name, it.Ready, it.Note)
		}
		if it.Duration != 1200 || it.Container == "" {
			t.Errorf("%s: 时长/容器未回填: %v %q", it.Name, it.Duration, it.Container)
		}
	}
}

func TestBuildShowsAutoMode(t *testing.T) {
	dir := filepath.Join("/media", "单剧")
	p1 := filepath.Join(dir, "S01E01.mkv")
	p2 := filepath.Join(dir, "S01E02.mkv")
	p3 := filepath.Join(dir, "S01E03.mkv")
	items := []scan.Item{
		itemOf(dir, "S01E01.mkv", 10, 1, 1),
		itemOf(dir, "S01E02.mkv", 10, 1, 2),
		itemOf(dir, "S01E03.mkv", 10, 1, 3),
	}
	res := map[string]auto.Result{
		p1: {Head: 21.5, Tail: 19.99, OK: true, Note: "自动识别"},
		p2: {Head: 21.5, Tail: 19.99, OK: true, Note: "自动识别"},
		p3: {OK: false, Note: "片尾静音不足"},
	}

	shows := BuildShows(items, nil, res, 0, 0)
	if len(shows) != 1 {
		t.Fatalf("应合成 1 部剧,实际 %d", len(shows))
	}
	sh := shows[0]
	if sh.Ready != 2 {
		t.Errorf("可执行集数 = %d, 期望 2", sh.Ready)
	}
	if !sh.Items[0].Ready || !sh.Items[1].Ready {
		t.Error("前两集应可执行")
	}
	if sh.Items[2].Ready {
		t.Error("识别失败的第三集不应可执行")
	}
	if !strings.HasPrefix(sh.Items[2].Note, "识别失败:") {
		t.Errorf("失败项 Note 应带前缀,实际 %q", sh.Items[2].Note)
	}
}

// res 非 nil 但缺少某一集时应标记为"未识别",而不是沿用手动值。
func TestBuildShowsMissingAutoResult(t *testing.T) {
	dir := "/media/x"
	p := filepath.Join(dir, "S01E01.mkv")
	shows := BuildShows([]scan.Item{itemOf(dir, "S01E01.mkv", 1, 1, 1)}, nil,
		map[string]auto.Result{}, 10, 10)
	it := shows[0].Items[0]
	if it.Ready {
		t.Errorf("%s 未参与识别却标记为可执行", p)
	}
	if it.Note != "未识别" {
		t.Errorf("Note = %q, 期望 未识别", it.Note)
	}
}

// 同一目录里混排多季时,应按 (目录, 季) 拆成独立分组。
func TestBuildShowsSplitsSeasons(t *testing.T) {
	dir := "/media/multi"
	items := []scan.Item{
		itemOf(dir, "S01E01.mkv", 1, 1, 1),
		itemOf(dir, "S02E01.mkv", 1, 2, 1),
	}
	shows := BuildShows(items, nil, nil, 5, 5)
	if len(shows) != 2 {
		t.Fatalf("混排多季应拆成 2 组,实际 %d", len(shows))
	}
	if shows[0].Season != 1 || shows[1].Season != 2 {
		t.Errorf("季号分组顺序错误: S%d, S%d", shows[0].Season, shows[1].Season)
	}
}

// 同一个目录里混排多部剧:应按剧名拆成独立分组,可以逐剧确认执行。
func TestBuildShowsSplitsMultipleShowsInOneDir(t *testing.T) {
	dir := "/media/混排"
	items := []scan.Item{
		itemOf(dir, "进击的巨人.S01E01.mkv", 100, 1, 1),
		itemOf(dir, "进击的巨人.S01E02.mkv", 100, 1, 2),
		itemOf(dir, "权力的游戏.S01E01.mkv", 300, 1, 1),
		itemOf(dir, "权力的游戏.S01E02.mkv", 300, 1, 2),
		itemOf(dir, "权力的游戏.S01E03.mkv", 300, 1, 3),
	}
	for i := range items {
		items[i].Show = scan.ShowOf(filepath.Base(items[i].Path))
	}

	shows := BuildShows(items, nil, nil, 30, 45)
	if len(shows) != 2 {
		t.Fatalf("同目录 2 部剧应拆成 2 组,实际 %d", len(shows))
	}
	// 按剧名排序:"权力的游戏" 与 "进击的巨人" 的字典序由 UTF-8 决定,
	// 这里只要求两组各自完整、互不混杂。
	byShow := map[string]Show{}
	for _, sh := range shows {
		byShow[sh.Show] = sh
	}
	a, ok := byShow["进击的巨人"]
	if !ok {
		t.Fatalf("缺剧名分组: %+v", shows)
	}
	if len(a.Items) != 2 || a.Ready != 2 || a.TotalIn != 200 {
		t.Errorf("进击的巨人: 集数=%d ready=%d 体积=%d", len(a.Items), a.Ready, a.TotalIn)
	}
	b := byShow["权力的游戏"]
	if len(b.Items) != 3 || b.Ready != 3 || b.TotalIn != 900 {
		t.Errorf("权力的游戏: 集数=%d ready=%d 体积=%d", len(b.Items), b.Ready, b.TotalIn)
	}
	if a.ID == b.ID {
		t.Error("两部剧的分组 ID 不应相同")
	}
	// 分组名带季号后缀,便于区分同名剧的不同季。
	if !strings.Contains(a.Name, "S01") {
		t.Errorf("分组名应带季号: %q", a.Name)
	}
}

// 剧名写法不同(点/下划线/大小写)应视为同一部剧。
func TestBuildShowsMergesNameVariants(t *testing.T) {
	dir := "/media/x"
	items := []scan.Item{
		itemOf(dir, "The.Show.S01E01.mkv", 1, 1, 1),
		itemOf(dir, "The-Show.S01E02.mkv", 1, 1, 2),
		itemOf(dir, "the_show.S01E03.mkv", 1, 1, 3),
	}
	for i := range items {
		items[i].Show = scan.ShowOf(filepath.Base(items[i].Path))
	}
	shows := BuildShows(items, nil, nil, 10, 10)
	if len(shows) != 1 {
		t.Fatalf("写法不同的同一部剧应合并成 1 组,实际 %d", len(shows))
	}
	if len(shows[0].Items) != 3 {
		t.Errorf("应有 3 集,实际 %d", len(shows[0].Items))
	}
}

// 不同季混排时没有季号的零散文件单独成组,且排在有季号的分组之后。
func TestBuildShowsUnnumberedSeasonLast(t *testing.T) {
	dir := "/media/mix"
	items := []scan.Item{
		itemOf(dir, "extra.mkv", 1, 0, 0),
		itemOf(dir, "S01E02.mkv", 1, 1, 2),
		itemOf(dir, "S01E01.mkv", 1, 1, 1),
	}
	shows := BuildShows(items, nil, nil, 5, 5)
	if len(shows) != 2 {
		t.Fatalf("应拆成「有季号」与「无季号」两组,实际 %d", len(shows))
	}
	if shows[0].Season != 1 || len(shows[0].Items) != 2 {
		t.Errorf("第一组应为 S01 且含 2 集,实际 S%d / %d 集",
			shows[0].Season, len(shows[0].Items))
	}
	if shows[1].Season != 0 || len(shows[1].Items) != 1 {
		t.Errorf("第二组应为无季号的零散文件,实际 S%d / %d 集",
			shows[1].Season, len(shows[1].Items))
	}
}

// 同一分组("第N集"这类无季号写法)里,没有集号的文件应排在本剧最后。
func TestBuildShowsUnnumberedLast(t *testing.T) {
	dir := "/media/mix"
	items := []scan.Item{
		itemOf(dir, "extra.mkv", 1, 0, 0),
		itemOf(dir, "第02集.mkv", 1, 0, 2),
		itemOf(dir, "第01集.mkv", 1, 0, 1),
	}
	shows := BuildShows(items, nil, nil, 5, 5)
	if len(shows) != 1 {
		t.Fatalf("同季应合成 1 组,实际 %d", len(shows))
	}
	if len(shows[0].Items) != 3 {
		t.Fatalf("应有 3 集,实际 %d", len(shows[0].Items))
	}
	if got := shows[0].Items[2].Name; got != "extra.mkv" {
		t.Errorf("无编号集应排最后,实际末项 %q", got)
	}
}

func TestBuildShowsEmpty(t *testing.T) {
	if shows := BuildShows(nil, nil, nil, 1, 1); len(shows) != 0 {
		t.Errorf("空输入应返回空列表,实际 %d", len(shows))
	}
}

func TestSummarize(t *testing.T) {
	shows := []Show{
		{
			ID: "a", Items: []Item{
				{Duration: 100, Ready: true}, {Duration: 200, Ready: false},
			},
			TotalIn: 1024,
		},
		{
			ID: "b", Items: []Item{
				{Duration: 50, Ready: true},
			},
			TotalIn: 2048,
		},
	}
	s := Summarize(shows)
	if s.Shows != 2 || s.Items != 3 {
		t.Errorf("shows=%d items=%d, 期望 2/3", s.Shows, s.Items)
	}
	if s.Ready != 2 {
		t.Errorf("ready=%d, 期望 2", s.Ready)
	}
	if s.TotalIn != 3072 {
		t.Errorf("total_in=%d, 期望 3072", s.TotalIn)
	}
	if s.DurSum != 350 {
		t.Errorf("duration_sum=%v, 期望 350", s.DurSum)
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.00 KiB"},
		{1536, "1.50 KiB"},
		{1024 * 1024, "1.00 MiB"},
		{3 * 1024 * 1024 * 1024, "3.00 GiB"},
	}
	for _, c := range cases {
		if got := HumanSize(c.in); got != c.want {
			t.Errorf("HumanSize(%d) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestTrimNote(t *testing.T) {
	if got := TrimNote("  正常\n备注  ", 20); got != "正常 备注" {
		t.Errorf("应压缩空白并去掉换行,实际 %q", got)
	}
	long := strings.Repeat("字", 10)
	got := TrimNote(long, 4)
	if len([]rune(got)) != 4 {
		t.Errorf("截断后长度 = %d, 期望 4 (%q)", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断应以省略号结尾: %q", got)
	}
	if got := TrimNote("短", 10); got != "短" {
		t.Errorf("未超长应原样返回,实际 %q", got)
	}
}

func TestSecs(t *testing.T) {
	cases := []struct {
		in   float64
		want time.Duration
	}{
		{0, 0},
		{-3, 0},
		{1.5, 1500 * time.Millisecond},
	}
	for _, c := range cases {
		if got := secs(c.in); got != c.want {
			t.Errorf("secs(%v) = %v, 期望 %v", c.in, got, c.want)
		}
	}
}

func TestBaseNameSeparators(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/a/b/c.mkv", "c.mkv"},
		{`C:\media\c.mkv`, "c.mkv"},
		{"plain.mkv", "plain.mkv"},
	}
	for _, c := range cases {
		if got := baseName(c.in); got != c.want {
			t.Errorf("baseName(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestTotalSize(t *testing.T) {
	items := []scan.Item{{Size: 10}, {Size: 20}, {Size: 30}}
	if got := totalSize(items); got != 60 {
		t.Errorf("totalSize = %d, 期望 60", got)
	}
}

// 剧名层级默认切点:手动模式取统一值,自动模式取就绪集众数,无就绪项归零。
func TestShowDefaultHeadTail(t *testing.T) {
	dir := "/media/def"

	// 手动模式:所有集共享 30/45,剧名层级默认即 30/45。
	sh := BuildShows([]scan.Item{
		itemOf(dir, "S01E01.mkv", 1, 1, 1),
		itemOf(dir, "S01E02.mkv", 1, 1, 2),
	}, nil, nil, 30, 45)[0]
	if sh.Head != 30 || sh.Tail != 45 {
		t.Errorf("手动模式剧名默认 = %v/%v, 期望 30/45", sh.Head, sh.Tail)
	}

	// 自动模式:两集识别为 21.5/19.99,一集失败(排除),众数即 22/20(四舍五入到秒)。
	res := map[string]auto.Result{
		filepath.Join(dir, "S01E01.mkv"): {Head: 21.5, Tail: 19.99, OK: true, Note: "自动识别"},
		filepath.Join(dir, "S01E02.mkv"): {Head: 21.5, Tail: 19.99, OK: true, Note: "自动识别"},
		filepath.Join(dir, "S01E03.mkv"): {OK: false, Note: "片尾静音不足"},
	}
	sh = BuildShows([]scan.Item{
		itemOf(dir, "S01E01.mkv", 1, 1, 1),
		itemOf(dir, "S01E02.mkv", 1, 1, 2),
		itemOf(dir, "S01E03.mkv", 1, 1, 3),
	}, nil, res, 0, 0)[0]
	if sh.Head != 22 || sh.Tail != 20 {
		t.Errorf("自动模式剧名默认 = %v/%v, 期望 22/20", sh.Head, sh.Tail)
	}

	// 全部未就绪(纯手动 0/0):剧名层级默认归零,用户需自行填写。
	sh = BuildShows([]scan.Item{itemOf(dir, "S01E01.mkv", 1, 1, 1)}, nil, nil, 0, 0)[0]
	if sh.Head != 0 || sh.Tail != 0 {
		t.Errorf("无默认切点剧名默认 = %v/%v, 期望 0/0", sh.Head, sh.Tail)
	}
}

// Locked 区分"禁止编辑"(识别失败)与"可编辑"(手动或识别成功)。
// 这是纯手动 Web 模式能用的前提:页面必须允许用户在 head/tail 为 0 的项上手动填值。
func TestBuildShowsLockSemantics(t *testing.T) {
	dir := "/media/lock"
	pOK := filepath.Join(dir, "ok.mkv")
	pFail := filepath.Join(dir, "fail.mkv")
	pManual := filepath.Join(dir, "manual.mkv")

	// 纯手动模式:res 为 nil,head/tail 都为 0——页面上这些项要可编辑,不能锁定。
	shows := BuildShows([]scan.Item{itemOf(dir, "manual.mkv", 1, 1, 1)},
		map[string]*ffmpeg.ProbeResult{pManual: probeOf(1200)}, nil, 0, 0)
	it := shows[0].Items[0]
	if it.Locked {
		t.Error("纯手动模式的项不应被锁定(否则页面无法编辑)")
	}
	if it.Ready {
		t.Error("无默认切点的纯手动项 Ready 应为 false")
	}

	// 手动模式带统一 head/tail:可编辑且 Ready。
	shows = BuildShows([]scan.Item{itemOf(dir, "manual.mkv", 1, 1, 1)},
		map[string]*ffmpeg.ProbeResult{pManual: probeOf(1200)}, nil, 30, 45)
	it = shows[0].Items[0]
	if it.Locked || !it.Ready {
		t.Errorf("手动+head/tail: Locked=%v Ready=%v, 期望 false/true", it.Locked, it.Ready)
	}

	// 自动识别:成功项可编辑,失败项锁定。
	res := map[string]auto.Result{
		pOK:   {Head: 21.5, Tail: 19.99, OK: true, Note: "自动识别"},
		pFail: {OK: false, Note: "片尾静音不足"},
	}
	shows = BuildShows([]scan.Item{
		itemOf(dir, "ok.mkv", 1, 1, 1),
		itemOf(dir, "fail.mkv", 1, 1, 2),
	}, map[string]*ffmpeg.ProbeResult{pOK: probeOf(1200), pFail: probeOf(1200)}, res, 0, 0)
	ok, fail := shows[0].Items[0], shows[0].Items[1]
	if ok.Locked || !ok.Ready {
		t.Errorf("识别成功项: Locked=%v Ready=%v, 期望 false/true", ok.Locked, ok.Ready)
	}
	if !fail.Locked || fail.Ready {
		t.Errorf("识别失败项: Locked=%v Ready=%v, 期望 true/false", fail.Locked, fail.Ready)
	}
	if !strings.HasPrefix(fail.Note, "识别失败:") {
		t.Errorf("失败项 Note 应带前缀,实际 %q", fail.Note)
	}
}
