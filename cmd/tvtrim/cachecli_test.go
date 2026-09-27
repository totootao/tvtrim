package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/totootao/tvtrim/internal/auto"
	"github.com/totootao/tvtrim/internal/dcache"
	"github.com/totootao/tvtrim/internal/scan"
)

// itemAt 造一个扫描项。
func itemAt(path, show string, season, episode int) scan.Item {
	return scan.Item{Path: path, Size: 64, Show: show, Season: season, Episode: episode}
}

// resultOf 造一个识别成功的结果。
func resultOf(head, tail float64, note string) auto.Result {
	return auto.Result{Head: head, Tail: tail, OK: true, Note: note}
}

// envOf 造一个只认预设值的环境变量读取函数。
func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// TestResolveCachePathExplicitWins 命令行 > 环境变量 > 环境默认值。
func TestResolveCachePathExplicitWins(t *testing.T) {
	inputs := []string{"/media"}
	env := envOf(map[string]string{envCacheFile: "/x/env.json"})
	// 命令行最优先。
	if got := resolveCachePath("/x/my.json", inputs, env, false); got != "/x/my.json" {
		t.Errorf("命令行应优先, 实际 %q", got)
	}
	// 其次环境变量。
	if got := resolveCachePath("", inputs, env, false); got != "/x/env.json" {
		t.Errorf("环境变量应生效, 实际 %q", got)
	}
}

// TestResolveCachePathContainer 容器里默认写进媒体目录 —— docker run --rm
// 每次都是新容器,写到家目录等于白写。
func TestResolveCachePathContainer(t *testing.T) {
	dir := t.TempDir()
	inputs := []string{dir}
	got := resolveCachePath("", inputs, func(string) string { return "" }, true)
	if want := filepath.Join(dir, cacheFileName); got != want {
		t.Errorf("容器内缓存路径 = %q, 期望 %q", got, want)
	}
	// 目标是文件时,取其所在目录。
	file := filepath.Join(dir, "a.mkv")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = resolveCachePath("", []string{file}, func(string) string { return "" }, true)
	if want := filepath.Join(dir, cacheFileName); got != want {
		t.Errorf("目标为文件时缓存路径 = %q, 期望 %q", got, want)
	}
}

// TestResolveCachePathHost 宿机上放进用户缓存目录,不往媒体库里塞东西。
func TestResolveCachePathHost(t *testing.T) {
	dir := t.TempDir()
	got := resolveCachePath("", []string{dir}, func(string) string { return "" }, false)
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Skip("无用户缓存目录")
	}
	if !strings.HasPrefix(got, cache) {
		t.Errorf("宿机缓存应在 %s 下, 实际 %q", cache, got)
	}
	if strings.Contains(got, dir) {
		t.Errorf("宿机缓存不该落在媒体目录里: %q", got)
	}
}

// TestOpenDetectCacheNoCache -no-cache 时压根不碰缓存。
func TestOpenDetectCacheNoCache(t *testing.T) {
	o := cliOptions{noCache: true}
	store, path := openDetectCache(o, []string{t.TempDir()}, "p")
	if store != nil || path != "" {
		t.Errorf("-no-cache 时不该载入缓存: store=%v path=%q", store, path)
	}
}

// TestOpenDetectCacheRefresh -refresh 要无视已有缓存,但保留路径以便写回。
func TestOpenDetectCacheRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	s := dcache.New("old-params")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}

	o := cliOptions{cacheFile: path, refresh: true}
	store, gotPath := openDetectCache(o, []string{dir}, "new-params")
	if gotPath != path {
		t.Errorf("缓存路径应保留以便写回, 实际 %q", gotPath)
	}
	if store == nil || len(store.Files) != 0 {
		t.Errorf("-refresh 应从空缓存开始, 实际 %v", store)
	}
	if store.Params != "new-params" {
		t.Errorf("-refresh 后应记录本次参数指纹, 实际 %q", store.Params)
	}
}

// TestOpenDetectCacheParamsChange 参数变了整份缓存失效,不混着用。
func TestOpenDetectCacheParamsChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	s := dcache.New("params-v1")
	s.Put(filepath.Join(dir, "a.mkv"), 1, time.Now(), dcache.Entry{Head: 30, OK: true})
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}

	// 同一参数:能接着用。
	store, _ := openDetectCache(cliOptions{cacheFile: path}, []string{dir}, "params-v1")
	if len(store.Files) != 1 {
		t.Errorf("同参数时应保留 1 条, 实际 %d", len(store.Files))
	}
	// 换了参数:整体重来。
	store, _ = openDetectCache(cliOptions{cacheFile: path}, []string{dir}, "params-v2")
	if len(store.Files) != 0 {
		t.Errorf("参数变了应整体失效, 实际还剩 %d 条", len(store.Files))
	}
}

// TestCacheInfoLine 终端说明要能说清"这次到底识别了什么"。
func TestCacheInfoLine(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		stat  detectStat
		total int
		want  string
	}{
		{"未启用缓存", "", detectStat{}, 5, ""},
		{"全部沿用", "/c.json", detectStat{FromCache: 5}, 5, "全部 5 个沿用上次结论"},
		{"部分沿用", "/c.json", detectStat{FromCache: 3, Detected: 2}, 5, "3 个沿用上次结论, 2 个新识别"},
		{"首次识别", "/c.json", detectStat{Detected: 5}, 5, "首次识别 5 个文件"},
	}
	for _, c := range cases {
		got := cacheInfoLine(c.path, c.stat, c.total)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: cacheInfoLine = %q, 应包含 %q", c.name, got, c.want)
		}
	}
}

// TestSaveDetectCacheRoundTrip 存进去的结论下次要能读出来,且切点一致。
func TestSaveDetectCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "S01E01.mkv")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "c.json")
	store := dcache.New("p")

	results := []detectResult{{
		Item: itemAt(file, "秀", 1, 1),
		Res:  resultOf(30, 45, "[一致 3/3] 片头切点 30.0s"),
	}}
	saveDetectCache(store, path, results)

	reloaded, err := dcache.Load(path)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	e, ok := reloaded.Lookup(file, 1)
	if !ok {
		t.Fatal("存进去的结论应能命中")
	}
	if e.Head != 30 || e.Tail != 45 {
		t.Errorf("切点 = %v/%v, 期望 30/45", e.Head, e.Tail)
	}
	if strings.Contains(e.Note, "一致") || strings.Contains(e.Note, "沿用") {
		t.Errorf("存盘的说明不该带前缀: %q", e.Note)
	}
	if e.Show != "秀" || e.Season != 1 {
		t.Errorf("剧名/季号未保留: %+v", e)
	}
}

// TestSaveDetectCacheSkipsFailures 识别失败的项不该写进缓存。
func TestSaveDetectCacheSkipsFailures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	a := filepath.Join(dir, "a.mkv")
	b := filepath.Join(dir, "b.mkv")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store := dcache.New("p")
	saveDetectCache(store, path, []detectResult{
		{Item: itemAt(a, "秀", 1, 1), Res: resultOf(30, 45, "x")},
		{Item: itemAt(b, "秀", 1, 2)}, // 未成功
	})
	if len(store.Files) != 1 {
		t.Errorf("只该存 1 条成功记录, 实际 %d", len(store.Files))
	}
}

// TestSaveDetectCacheNoStore 未启用缓存时保存应是空操作(不 panic、不落盘)。
func TestSaveDetectCacheNoStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	saveDetectCache(nil, path, nil)
	saveDetectCache(dcache.New("p"), "", nil)
	if _, err := os.Stat(path); err == nil {
		t.Error("不该写出缓存文件")
	}
}
