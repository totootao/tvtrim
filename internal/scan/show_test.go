package scan

import (
	"path/filepath"
	"testing"
)

func TestShowOf(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		// 英文剧:SxxExx
		{"Show.S01E01.1080p.WEB-DL.x264-GRP.mkv", "Show"},
		{"Game.Of.Thrones.S01E02.720p.mkv", "Game Of Thrones"},
		{"The Show - S02E03 - 1080p.mkv", "The Show"},
		{"[Group] Show - S03E07 [1080p].mkv", "Show"},
		{"show.s1e5.avi", "show"},
		{"Show_S01E01_1080p.mp4", "Show"},

		// 中文剧:SxxExx
		{"进击的巨人.S01E01.1080p.mkv", "进击的巨人"},
		{"进击的巨人.S01E12.mp4", "进击的巨人"},
		{"某剧.S02E08.EP99.mkv", "某剧"},

		// 中文剧:第N集 / 第N话
		{"某剧.第01集.mp4", "某剧"},
		{"某剧 第24集 1080p.mkv", "某剧"},
		{"动画 第100话.mp4", "动画"},

		// EPxx / Exx
		{"Show.EP05.1080p.mp4", "Show"},
		{"show-ep12.mkv", "show"},
		{"Show.E07.mkv", "Show"},

		// 年份与技术标签要被清掉
		{"The Show 2019 S01E01 1080p WEB-DL.mkv", "The Show"},
		{"Show.2020.S01E01.mkv", "Show"},

		// 解析不出剧名:返回空串,由调用方回退到目录名
		{"S01E01.mkv", ""},
		{"第01集.mkv", ""},
		{"EP01.mkv", ""},
		{"random_video.mp4", ""},
		{"movie.mp4", ""},
		{"season.mp4", ""},
		{"series.mp4", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := ShowOf(c.name); got != c.want {
			t.Errorf("ShowOf(%q) = %q, 期望 %q", c.name, got, c.want)
		}
	}
}

// 剧名含数字或"部分像季集标记"的边界情况。
func TestShowOfEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"进击的巨人2.S01E01.mkv", "进击的巨人2"}, // 剧名带序号要保留
		{"Sesame Street S01E01.mkv", "Sesame Street"},
		{"Mr.Robot.S01E01.mkv", "Mr Robot"}, // 点视为分隔符
		{"Show.1080p.S01E01.mkv", "Show"},   // 尾部的技术标签一律剥离
		{"Show.Part2.E01.mkv", "Show Part2"},
	}
	for _, c := range cases {
		if got := ShowOf(c.name); got != c.want {
			t.Errorf("ShowOf(%q) = %q, 期望 %q", c.name, got, c.want)
		}
	}
}

func TestShowKeyNormalizes(t *testing.T) {
	pairs := [][2]string{
		{"Show Name", "show name"},
		{"Show.Name", "show name"},
		{"Show-Name", "show name"},
		{"Show_Name", "show name"},
		{"SHOW NAME", "show name"},
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		// 同一部剧的不同写法应得到相同的分组键。
		a := ShowKey(pairs[i][0])
		b := ShowKey(pairs[i][1])
		if a != b {
			t.Errorf("ShowKey(%q)=%q 与 ShowKey(%q)=%q 应相同", pairs[i][0], a, pairs[i][1], b)
		}
	}
	if ShowKey("") != "" {
		t.Error("空剧名的键也应为空")
	}
	// 中文剧名不受大小写/分隔符影响。
	if ShowKey("进击的巨人") != ShowKey("进击的巨人") {
		t.Error("中文剧名键应稳定")
	}
}

func TestCountShows(t *testing.T) {
	dir := "/media/剧集"
	items := []Item{
		{Path: dir + "/A.S01E01.mkv", Show: ShowOf("A.S01E01.mkv")},
		{Path: dir + "/A.S01E02.mkv", Show: ShowOf("A.S01E02.mkv")},
		{Path: dir + "/B.S01E01.mkv", Show: ShowOf("B.S01E01.mkv")},
		{Path: dir + "/B.S02E01.mkv", Show: ShowOf("B.S02E01.mkv")},
		{Path: dir + "/random.mp4", Show: ShowOf("random.mp4")},
		{Path: dir + "/other.mp4", Show: ShowOf("other.mp4")},
	}
	// A 一部、B 一部,两个无剧名文件按同一目录算一部 = 3。
	if got := CountShows(items); got != 3 {
		t.Errorf("CountShows = %d, 期望 3", got)
	}
	if got := CountShows(nil); got != 0 {
		t.Errorf("空输入应为 0,实际 %d", got)
	}
}

// 扫描时就要把剧名填好,后续分组与纠错都依赖它。
func TestCollectFillsShow(t *testing.T) {
	dir := t.TempDir()
	mkVideo(t, dir, "进击的巨人.S01E01.mkv", "权力的游戏.S01E01.mkv", "no-name.mkv")

	items, err := Collect([]string{dir}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("数量 = %d", len(items))
	}
	byName := map[string]string{}
	for _, it := range items {
		byName[filepath.Base(it.Path)] = it.Show
	}
	if byName["进击的巨人.S01E01.mkv"] != "进击的巨人" {
		t.Errorf("剧名 = %q", byName["进击的巨人.S01E01.mkv"])
	}
	if byName["权力的游戏.S01E01.mkv"] != "权力的游戏" {
		t.Errorf("剧名 = %q", byName["权力的游戏.S01E01.mkv"])
	}
	// 文件名解析不出剧名时,退回到所在目录名(而不是留空)。
	if want := CleanShow(filepath.Base(dir)); byName["no-name.mkv"] != want {
		t.Errorf("无剧名文件应借用目录名 %q, 实际 %q", want, byName["no-name.mkv"])
	}
}

// TestCollectFillsShowAcrossSeasonDirs 覆盖 剧名/季/集 的两级布局:
// 文件名没有剧名时,应跳过纯季目录向上取到真正的剧名。
func TestCollectFillsShowAcrossSeasonDirs(t *testing.T) {
	root := t.TempDir()
	s1 := filepath.Join(root, "进击的巨人", "S01")
	s2 := filepath.Join(root, "进击的巨人", "S02")
	mkVideo(t, s1, "S01E01.mkv", "S01E02.mkv")
	mkVideo(t, s2, "S02E01.mkv")

	items, err := Collect([]string{root}, Options{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("数量 = %d", len(items))
	}
	for _, it := range items {
		if it.Show != "进击的巨人" {
			t.Errorf("%s 的剧名 = %q, 需要 进击的巨人", filepath.Base(it.Path), it.Show)
		}
	}
	// 两季应算同一部剧。
	if got := CountShows(items); got != 1 {
		t.Errorf("CountShows = %d, 应为 1(两季同属一部剧)", got)
	}
}

func TestShowOfPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		// 文件名里有剧名时,永远以文件名为准。
		{"/media/进击的巨人.S01E01.mkv", "进击的巨人"},
		{"/media/剧A/进击的巨人.S01E01.mkv", "进击的巨人"},
		{"/media/进击的巨人/S01/S01E01.mkv", "进击的巨人"},
		// 文件名没剧名 → 借用父目录名。
		{"/media/进击的巨人.S01/S01E01.mkv", "进击的巨人"},
		{"/media/The.Show.1080p/S01E01.mkv", "The Show"},
		{"/media/权力的游戏/S01E01.mkv", "权力的游戏"},
		// 父目录是纯季目录 → 跳过它,用祖父目录名。
		{"/media/进击的巨人/S01/E01.mkv", "进击的巨人"},
		{"/media/进击的巨人/S02/E01.mkv", "进击的巨人"},
		{"/media/权力的游戏/Season 3/E01.mkv", "权力的游戏"},
		{"/media/权力的游戏/Season3/E01.mkv", "权力的游戏"},
		{"/media/权力的游戏/第1季/E01.mkv", "权力的游戏"},
		// 目录名本身就趴在根上:没有更多层级可借,保留目录名。
		{"/S01/E01.mkv", "S01"},
	}
	for _, c := range cases {
		if got := ShowOfPath(c.path); got != c.want {
			t.Errorf("ShowOfPath(%q) = %q, 需要 %q", c.path, got, c.want)
		}
	}
}

// TestShowOfPathKeepsSeasonInDirName 验证非季目录不会被误判跳过。
func TestShowOfPathKeepsSeasonInDirName(t *testing.T) {
	if got := ShowOfPath("/media/权力的游戏.S03/S03E01.mkv"); got != "权力的游戏" {
		t.Errorf("目录名里的季标记应被剥掉,实际 %q", got)
	}
}

func TestCleanShowStripsSeasonSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"进击的巨人.S01", "进击的巨人"},
		{"权力的游戏 Season 2", "权力的游戏"},
		{"权力的游戏 第2季", "权力的游戏"},
		{"权力的游戏 S02 1080p", "权力的游戏"},
	}
	for _, c := range cases {
		if got := CleanShow(c.in); got != c.want {
			t.Errorf("CleanShow(%q) = %q, 需要 %q", c.in, got, c.want)
		}
	}
}

// TestCleanShowKeepsLoneSeasonToken 只有季标记一个词时不剥,否则剧名会空掉。
func TestCleanShowKeepsLoneSeasonToken(t *testing.T) {
	if got := CleanShow("S01"); got != "S01" {
		t.Errorf("CleanShow(\"S01\") = %q, 应保留", got)
	}
}
