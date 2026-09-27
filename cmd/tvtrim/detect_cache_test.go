package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/totootao/tvtrim/internal/dcache"
	"github.com/totootao/tvtrim/internal/ffmpeg"
	"github.com/totootao/tvtrim/internal/scan"
	"github.com/totootao/tvtrim/internal/testkit"
)

// newNoSilenceRunner 造一个"探测正常、静音检测必然失败"的 ffmpeg。
// 用它才能区分结论的两种来源:命中缓存的文件照样有结果,
// 需要现场解码的文件一个都识别不出来。
func newNoSilenceRunner(t *testing.T) *ffmpeg.Runner {
	t.Helper()
	root := t.TempDir()
	ff := testkit.WriteFakeFFmpeg(t, root)
	testkit.SetEnv(t, testkit.EnvOut, testkit.WriteFile(t, root, "stderr.txt", combinedStderr))
	testkit.SetEnv(t, testkit.EnvMode, testkit.ModeNoSilence)
	return &ffmpeg.Runner{Path: ff}
}

// cacheItem 把某个文件以指定切点写进缓存(size/mtime 取真实值)。
func cacheItem(t *testing.T, store *dcache.Store, path string, head, tail float64) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("无法读取 %s: %v", path, err)
	}
	store.Put(path, info.Size(), info.ModTime(), dcache.Entry{
		Head: head, Tail: tail, OK: true, Note: "片头切点 30.0s",
	})
}

// TestDetectAllOnlyDetectsNewFiles 是这次改动的核心:
// 6 集里 4 集有缓存,本次只该解码新增的 2 集。
func TestDetectAllOnlyDetectsNewFiles(t *testing.T) {
	runner := newRunner(t)
	items := sampleItems(t, 6)

	cache := dcache.New("p")
	for _, it := range items[:4] {
		cacheItem(t, cache, it.Path, 30, 45)
	}

	got, stat := detectAllWithStat(context.Background(), runner, items,
		detectConfig{Mode: DetectSilence, Sample: 3, Cache: cache})

	if stat.FromCache != 4 {
		t.Errorf("沿用缓存的文件数 = %d, 期望 4", stat.FromCache)
	}
	if stat.Detected != 2 {
		t.Errorf("本次静音检测的文件数 = %d, 期望 2(只有新增的两集)", stat.Detected)
	}
	if len(got) != 6 {
		t.Fatalf("结果数 = %d, 期望 6", len(got))
	}
	// 缓存里的 4 集占多数,新增 2 集虽有实测值也应服从本剧结论。
	for _, d := range got {
		if !d.OK() {
			t.Fatalf("%s 应有结论, 实际失败", d.Item.Path)
		}
		if d.Res.Head != 30 || d.Res.Tail != 45 {
			t.Errorf("%s 切点 = %v/%v, 期望 30/45", d.Item.Path, d.Res.Head, d.Res.Tail)
		}
	}
}

// TestDetectAllCacheSurvivesSilenceFailure 反向验证:
// 同一批文件在静音检测必然失败时,有缓存的集依然有结论。
func TestDetectAllCacheSurvivesSilenceFailure(t *testing.T) {
	runner := newNoSilenceRunner(t)
	items := sampleItems(t, 3)

	// 无缓存:一个都识别不出来。
	plain, stat := detectAllWithStat(context.Background(), runner, items,
		detectConfig{Mode: DetectSilence, Sample: 3})
	if stat.FromCache != 0 {
		t.Errorf("没给缓存却命中了 %d 条", stat.FromCache)
	}
	for _, d := range plain {
		if d.OK() {
			t.Fatalf("%s: 静音检测失败时不应有结论", d.Item.Path)
		}
	}

	// 有缓存:命中的那一集照样有切点,而且是缓存里的值。
	cache := dcache.New("p")
	cacheItem(t, cache, items[0].Path, 30, 45)
	got, stat := detectAllWithStat(context.Background(), runner, items,
		detectConfig{Mode: DetectSilence, Sample: 3, Cache: cache})
	if stat.FromCache != 1 {
		t.Errorf("沿用缓存的文件数 = %d, 期望 1", stat.FromCache)
	}
	if !got[0].OK() || got[0].Res.Head != 30 || got[0].Res.Tail != 45 {
		t.Errorf("命中缓存的集应沿用缓存切点, 实际 %+v", got[0].Res)
	}
	if !strings.Contains(got[0].Res.Note, "沿用上次识别") {
		t.Errorf("说明应标明结论来自缓存, 实际 %q", got[0].Res.Note)
	}
}

// TestDetectAllCacheIgnoresChangedFile 文件换过了就不能再沿用旧切点。
func TestDetectAllCacheIgnoresChangedFile(t *testing.T) {
	runner := newNoSilenceRunner(t)
	items := sampleItems(t, 1)
	cache := dcache.New("p")
	cacheItem(t, cache, items[0].Path, 30, 45)

	// 把文件改成别的内容(size/mtime 都变),缓存应失效。
	if err := os.WriteFile(items[0].Path, []byte("完全不同的一段内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, stat := detectAllWithStat(context.Background(), runner, items,
		detectConfig{Mode: DetectSilence, Sample: 3, Cache: cache})
	if stat.FromCache != 0 {
		t.Errorf("文件变过却仍沿用了 %d 条缓存", stat.FromCache)
	}
	if got[0].OK() {
		t.Errorf("缓存失效后就不该有结论, 实际 %+v", got[0].Res)
	}
}

// TestDetectAllCacheFullHit 全部命中时一次静音检测都不做。
func TestDetectAllCacheFullHit(t *testing.T) {
	runner := newNoSilenceRunner(t) // 真去检测必然失败,所以下面的成功只可能来自缓存
	items := sampleItems(t, 4)
	cache := dcache.New("p")
	for _, it := range items {
		cacheItem(t, cache, it.Path, 30, 45)
	}

	got, stat := detectAllWithStat(context.Background(), runner, items,
		detectConfig{Mode: DetectSilence, Sample: 3, Cache: cache})
	if stat.FromCache != 4 {
		t.Errorf("沿用缓存的文件数 = %d, 期望 4", stat.FromCache)
	}
	if stat.Detected != 0 {
		t.Errorf("全部命中时检测数应为 0, 实际 %d", stat.Detected)
	}
	for _, d := range got {
		if !d.OK() {
			t.Errorf("%s 应沿用缓存结论", d.Item.Path)
		}
	}
}

// TestDetectAllProbeOnlyIgnoresCache 只探测模式下不该读缓存(那模式压根不识别切点)。
func TestDetectAllProbeOnlyIgnoresCache(t *testing.T) {
	runner := newRunner(t)
	items := sampleItems(t, 2)
	cache := dcache.New("p")
	cacheItem(t, cache, items[0].Path, 30, 45)

	got, stat := detectAllWithStat(context.Background(), runner, items,
		detectConfig{Mode: DetectProbeOnly, Cache: cache})
	if stat.FromCache != 0 || stat.Detected != 0 {
		t.Errorf("只探测模式不参与缓存: from=%d detected=%d", stat.FromCache, stat.Detected)
	}
	for _, d := range got {
		if d.Res.OK {
			t.Errorf("%s: 只探测模式不该有切点结论", d.Item.Path)
		}
		if d.Probe == nil {
			t.Errorf("%s: 只探测模式应保留探测结果", d.Item.Path)
		}
	}
}

// TestDetectAllCacheAcrossShows 缓存按文件独立生效:
// 一部剧有缓存、另一部没有时,各自走各自的路径,互不干扰。
func TestDetectAllCacheAcrossShows(t *testing.T) {
	dir := t.TempDir()
	mkItems := func(show string, n int) []scan.Item {
		var items []scan.Item
		for i := 1; i <= n; i++ {
			name := fmt.Sprintf("%s.S01E%02d.mkv", show, i)
			p := testkit.WriteFile(t, dir, name, "x")
			items = append(items, scan.Item{Path: p, Size: 64, Show: show, Season: 1, Episode: i})
		}
		return items
	}
	// A 剧 3 集全部有缓存,B 剧 3 集从没见过。
	a := mkItems("剧A", 3)
	b := mkItems("剧B", 3)
	items := append(append([]scan.Item{}, a...), b...)

	cache := dcache.New("p")
	for _, it := range a {
		cacheItem(t, cache, it.Path, 30, 45)
	}

	got, stat := detectAllWithStat(context.Background(), newRunner(t), items,
		detectConfig{Mode: DetectSilence, Sample: 3, Cache: cache})
	if stat.FromCache != 3 {
		t.Errorf("沿用缓存的文件数 = %d, 期望 3(只有剧A)", stat.FromCache)
	}
	if stat.Detected != 3 {
		t.Errorf("本次检测的文件数 = %d, 期望 3(只有剧B)", stat.Detected)
	}
	byPath := map[string]detectResult{}
	for _, d := range got {
		byPath[d.Item.Path] = d
	}
	for _, it := range a {
		if got := byPath[it.Path]; got.Res.Head != 30 || got.Res.Tail != 45 {
			t.Errorf("%s 应沿用本剧缓存切点, 实际 %v/%v", it.Path, got.Res.Head, got.Res.Tail)
		}
	}
	for _, it := range b {
		if got := byPath[it.Path]; got.Res.Head != 96.5 {
			t.Errorf("%s 应现场检测出 96.5s, 实际 %v", it.Path, got.Res.Head)
		}
	}
}
