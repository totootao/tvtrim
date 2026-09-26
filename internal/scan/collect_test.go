package scan

import (
	"os"
	"path/filepath"
	"testing"
)

// mkVideo 在 dir 下创建若干媒体文件与干扰文件。
func mkVideo(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCollectFiltersByExt(t *testing.T) {
	dir := t.TempDir()
	mkVideo(t, dir, "a.mp4", "b.mkv", "c.txt", "readme.md", "d.webm")
	items, err := Collect([]string{dir}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("期望 3 个媒体文件,得到 %d: %+v", len(items), items)
	}
}

func TestCollectExcludesOutputFiles(t *testing.T) {
	dir := t.TempDir()
	mkVideo(t, dir, "ep01.mp4", "ep01-trim.mp4")
	items, err := Collect([]string{dir}, Options{ExcludeSuffix: "-trim"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || filepath.Base(items[0].Path) != "ep01.mp4" {
		t.Fatalf("应排除上次输出: %+v", items)
	}
}

func TestCollectSkipsHiddenAndPartial(t *testing.T) {
	dir := t.TempDir()
	mkVideo(t, dir, "ep01.mp4", ".hidden.mp4", "part.mkv.part")
	items, err := Collect([]string{dir}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("应跳过隐藏文件与 .part: %+v", items)
	}
}

func TestCollectRecursive(t *testing.T) {
	dir := t.TempDir()
	mkVideo(t, dir, "top.mp4", "S01/e1.mkv", "S01/e2.mkv")

	flat, err := Collect([]string{dir}, Options{})
	if err != nil || len(flat) != 1 {
		t.Fatalf("非递归只应拿顶层: %d %v", len(flat), err)
	}
	deep, err := Collect([]string{dir}, Options{Recursive: true})
	if err != nil || len(deep) != 3 {
		t.Fatalf("递归应拿 3 个: %d %v", len(deep), err)
	}
}

func TestCollectExplicitFileIgnoresExt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "weird.mpeg")
	mkVideo(t, dir, "weird.mpeg")
	items, err := Collect([]string{p}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("显式指定的文件应被接受(不受白名单限制): %d", len(items))
	}
}

func TestCollectSortsBySeasonEpisode(t *testing.T) {
	dir := t.TempDir()
	mkVideo(t, dir, "S01E03.mkv", "S01E01.mkv", "S02E01.mkv", "no-number.mkv")
	items, err := Collect([]string{dir}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		file       string
		season, ep int
	}{
		{"S01E01.mkv", 1, 1}, {"S01E03.mkv", 1, 3}, {"S02E01.mkv", 2, 1}, {"no-number.mkv", 0, 0},
	}
	if len(items) != len(want) {
		t.Fatalf("数量 = %d", len(items))
	}
	for i, w := range want {
		if filepath.Base(items[i].Path) != w.file {
			t.Errorf("位置 %d: 得到 %s, 期望 %s", i, filepath.Base(items[i].Path), w.file)
		}
		if items[i].Season != w.season || items[i].Episode != w.ep {
			t.Errorf("位置 %d (%s): S%dE%d, 期望 S%dE%d",
				i, w.file, items[i].Season, items[i].Episode, w.season, w.ep)
		}
	}
}

func TestCollectMissingPath(t *testing.T) {
	if _, err := Collect([]string{filepath.Join(t.TempDir(), "nope")}, Options{}); err == nil {
		t.Error("路径不存在应报错")
	}
}

func TestParseEpisodeExtraPatterns(t *testing.T) {
	cases := []struct {
		name       string
		season, ep int
	}{
		{"一见钟情.S01E12.mp4", 1, 12},
		{"第02集.mkv", 0, 2},
		{"第05话.mp4", 0, 5},
		{"EP07.mkv", 0, 7},
		{"something-else.mkv", 0, 0},
	}
	for _, c := range cases {
		s, e := ParseEpisode(c.name)
		if s != c.season || e != c.ep {
			t.Errorf("ParseEpisode(%q) = S%dE%d, 期望 S%dE%d", c.name, s, e, c.season, c.ep)
		}
	}
}
