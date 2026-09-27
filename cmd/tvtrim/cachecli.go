package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/totootao/tvtrim/internal/dcache"
)

// 缓存相关的默认值与环境变量。
const (
	// envCacheFile 可以指定缓存文件位置,与 -cache-file 等价(命令行优先)。
	envCacheFile = "TVTRIM_CACHE_FILE"
	// cacheFileName 是容器内默认写在媒体目录里的隐藏文件名。
	// 用点号开头,媒体服务器(Plex/Jellyfin 等)的刮削一般会忽略。
	cacheFileName = ".tvtrim-cache.json"
)

// resolveCachePath 决定识别缓存放在哪。
//
// 优先级:命令行 -cache-file > 环境变量 > 容器默认 > 宿机默认。
//
// 容器里默认写进**首个目标目录**:docker run --rm 每次都是全新容器,
// 写到 ~/.cache 等于白写,而挂载进来的媒体目录是唯一能留下来的地方。
// 宿机上反过来,写进用户的缓存目录,不往媒体库里塞东西。
func resolveCachePath(explicit string, inputs []string, getenv func(string) string, container bool) string {
	if explicit != "" {
		return explicit
	}
	if v := getenv(envCacheFile); v != "" {
		return v
	}
	if container {
		if dir := firstTargetDir(inputs); dir != "" {
			return filepath.Join(dir, cacheFileName)
		}
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "tvtrim", "detect.json")
	}
	return filepath.Join(os.TempDir(), "tvtrim-detect.json")
}

// firstTargetDir 给出首个目标所在的目录:目录就是它本身,文件取其父目录。
func firstTargetDir(inputs []string) string {
	for _, in := range inputs {
		if in == "" {
			continue
		}
		abs, err := filepath.Abs(in)
		if err != nil {
			abs = in
		}
		if info, err := os.Stat(abs); err == nil && info.IsDir() {
			return abs
		}
		return filepath.Dir(abs)
	}
	return ""
}

// openDetectCache 按选项载入缓存。
//
// 返回 nil 表示本次不使用缓存(-no-cache)。params 是本次识别的参数指纹,
// 与缓存里记录的不一致时整份失效 —— 免得把旧参数算出的切点当成这次的结论。
// refresh 为真时同样从空缓存开始,但结束时仍会写回。
func openDetectCache(o cliOptions, inputs []string, params string) (*dcache.Store, string) {
	if o.noCache {
		return nil, ""
	}
	path := resolveCachePath(o.cacheFile, inputs, os.Getenv, inContainer())
	store, err := dcache.Load(path)
	if err != nil {
		fmt.Printf("提示: 识别缓存读取失败(%v),本次将重新识别。\n", err)
		store = dcache.New(params)
	}
	if o.refresh {
		store = dcache.New(params)
	}
	if !store.Usable(params) {
		// 参数变了或结构升级:整份重来,而不是混着用。
		store = dcache.New(params)
	}
	return store, path
}

// saveDetectCache 把本次识别成功的结论写回缓存。
// 写失败只提示不报错 —— 缓存丢了顶多多花点时间,不该让裁剪任务失败。
func saveDetectCache(store *dcache.Store, path string, results []detectResult) {
	if store == nil || path == "" {
		return
	}
	n := 0
	for _, d := range results {
		if !d.OK() {
			continue
		}
		info, err := os.Stat(d.Item.Path)
		if err != nil {
			continue
		}
		store.Put(d.Item.Path, info.Size(), info.ModTime(), dcache.Entry{
			Head:   d.Res.Head,
			Tail:   d.Res.Tail,
			OK:     true,
			Note:   dcache.CleanNote(d.Res.Note),
			Show:   d.Item.Show,
			Season: d.Item.Season,
		})
		n++
	}
	if n == 0 {
		return
	}
	if err := store.Save(path); err != nil {
		fmt.Printf("提示: 识别缓存写入失败(%v),下次仍需重新识别。\n", err)
	}
}

// cacheInfoLine 给出缓存的终端说明,让"这次到底识别了什么"一目了然。
func cacheInfoLine(path string, stat detectStat, total int) string {
	if path == "" {
		return ""
	}
	switch {
	case stat.FromCache > 0 && stat.Detected > 0:
		return fmt.Sprintf("识别缓存: %s(%d 个沿用上次结论, %d 个新识别)", path, stat.FromCache, stat.Detected)
	case stat.FromCache > 0:
		return fmt.Sprintf("识别缓存: %s(全部 %d 个沿用上次结论,无需重新识别)", path, stat.FromCache)
	default:
		return fmt.Sprintf("识别缓存: %s(首次识别 %d 个文件)", path, total)
	}
}
