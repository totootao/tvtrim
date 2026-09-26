package ffmpeg

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeSizedExec 造一个指定字节数的"可执行"文件(稀疏写入,避免真占空间)。
func writeSizedExec(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// reportTo 收集 Locate 的进度消息,便于断言走到了哪条分支。
func reportTo(dst *[]string) func(string) {
	return func(msg string) { *dst = append(*dst, msg) }
}

// assetServer 起一个假下载服务:返回指定字节数的二进制。
func assetServer(t *testing.T, size int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if size > 0 {
			w.Header().Set("Content-Type", "application/octet-stream")
			if _, err := w.Write(make([]byte, size)); err != nil {
				t.Errorf("写入响应失败: %v", err)
			}
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func currentAsset(t *testing.T) asset {
	t.Helper()
	a, err := pickAsset()
	if err != nil {
		t.Skipf("当前平台 %s/%s 无对应资产: %v", runtime.GOOS, runtime.GOARCH, err)
	}
	return a
}

func TestCheckExecutable(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ffmpeg")
	writeSizedExec(t, ok, 8)

	if err := checkExecutable(ok); err != nil {
		t.Errorf("合法可执行文件应通过校验: %v", err)
	}
	if err := checkExecutable(filepath.Join(dir, "missing")); err == nil {
		t.Error("不存在的路径应报错")
	}
	if err := checkExecutable(dir); err == nil {
		t.Error("目录应报错")
	}
	noPerm := filepath.Join(dir, "no-perm")
	if err := os.WriteFile(noPerm, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := checkExecutable(noPerm); err == nil {
			t.Error("没有可执行权限的文件应报错")
		}
	}
}

func TestLocateExplicit(t *testing.T) {
	dir := t.TempDir()
	rel := filepath.Join(dir, "my-ffmpeg")
	writeSizedExec(t, rel, 8)

	runner, err := Locate(context.Background(), LocateOptions{Explicit: rel})
	if err != nil {
		t.Fatalf("显式指定应成功: %v", err)
	}
	if !filepath.IsAbs(runner.Path) {
		t.Errorf("应返回绝对路径,实际 %q", runner.Path)
	}
	if _, err := os.Stat(runner.Path); err != nil {
		t.Errorf("返回的路径不可用: %v", err)
	}
}

func TestLocateExplicitRejectsBadPath(t *testing.T) {
	if _, err := Locate(context.Background(), LocateOptions{
		Explicit: filepath.Join(t.TempDir(), "nope"),
	}); err == nil || !strings.Contains(err.Error(), "--ffmpeg 指定的文件不可用") {
		t.Errorf("错误提示不符: %v", err)
	}
}

func TestLocateEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "env-ffmpeg")
	writeSizedExec(t, p, 8)
	t.Setenv("TVTRIM_FFMPEG", p)

	runner, err := Locate(context.Background(), LocateOptions{CacheDir: dir})
	if err != nil {
		t.Fatalf("环境变量定位失败: %v", err)
	}
	if runner.Path != p {
		t.Errorf("路径 = %q, 期望 %q", runner.Path, p)
	}

	t.Setenv("TVTRIM_FFMPEG", filepath.Join(dir, "ghost"))
	if _, err := Locate(context.Background(), LocateOptions{CacheDir: dir}); err == nil ||
		!strings.Contains(err.Error(), "TVTRIM_FFMPEG 指定的文件不可用") {
		t.Errorf("错误提示不符: %v", err)
	}
}

// Locate 的优先级:显式路径应盖过环境变量。
func TestLocateExplicitBeatsEnv(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "a")
	writeSizedExec(t, explicit, 8)
	t.Setenv("TVTRIM_FFMPEG", filepath.Join(dir, "b"))

	runner, err := Locate(context.Background(), LocateOptions{Explicit: explicit})
	if err != nil {
		t.Fatal(err)
	}
	if runner.Path != explicit {
		t.Errorf("路径 = %q, 期望显式指定的 %q", runner.Path, explicit)
	}
}

func TestLocateUsesFreshCache(t *testing.T) {
	a := currentAsset(t)
	cache := t.TempDir()
	cached := filepath.Join(cache, cacheBinaryName())
	writeSizedExec(t, cached, a.Size)

	var msgs []string
	runner, err := Locate(context.Background(), LocateOptions{CacheDir: cache, Progress: reportTo(&msgs)})
	if err != nil {
		t.Fatalf("命中缓存失败: %v", err)
	}
	if runner.Path != cached {
		t.Errorf("路径 = %q, 期望缓存路径 %q", runner.Path, cached)
	}
	if len(msgs) == 0 || !strings.Contains(msgs[0], "使用缓存的 ffmpeg-trim") {
		t.Errorf("进度消息不符: %+v", msgs)
	}
}

// 缓存字节数与资产清单不符时视为过期:删掉重新下载。
func TestLocateReplacesStaleCache(t *testing.T) {
	a := currentAsset(t)
	cache := t.TempDir()
	cached := filepath.Join(cache, cacheBinaryName())
	writeSizedExec(t, cached, a.Size+123) // 旧版本

	srv := assetServer(t, a.Size)
	t.Setenv("TVTRIM_PROXY", srv.URL)

	var msgs []string
	runner, err := Locate(context.Background(), LocateOptions{CacheDir: cache, Progress: reportTo(&msgs)})
	if err != nil {
		t.Fatalf("重新下载失败: %v", err)
	}
	joined := strings.Join(msgs, "\n")
	if !strings.Contains(joined, "已过期") {
		t.Errorf("应提示缓存过期: %s", joined)
	}
	if !strings.Contains(joined, "下载完成") {
		t.Errorf("应提示下载完成: %s", joined)
	}
	st, err := os.Stat(runner.Path)
	if err != nil {
		t.Fatalf("下载后的文件不可用: %v", err)
	}
	if st.Size() != a.Size {
		t.Errorf("下载后体积 = %d, 期望 %d", st.Size(), a.Size)
	}
	// 第二次调用应直接命中缓存,不再下载。
	msgs = nil
	if _, err := Locate(context.Background(), LocateOptions{CacheDir: cache, Progress: reportTo(&msgs)}); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0], "使用缓存") {
		t.Errorf("二次调用应命中缓存: %+v", msgs)
	}
}

func TestLocateDownloadsWhenCacheMissing(t *testing.T) {
	a := currentAsset(t)
	cache := t.TempDir()
	srv := assetServer(t, a.Size)
	t.Setenv("TVTRIM_PROXY", srv.URL)

	var msgs []string
	runner, err := Locate(context.Background(), LocateOptions{CacheDir: cache, Progress: reportTo(&msgs)})
	if err != nil {
		t.Fatalf("首次下载失败: %v", err)
	}
	if filepath.Dir(runner.Path) != cache {
		t.Errorf("下载应落到缓存目录,实际 %q", runner.Path)
	}
	if !strings.Contains(strings.Join(msgs, "\n"), "自动下载") {
		t.Errorf("应提示开始下载: %+v", msgs)
	}
	// 元信息文件应被写入。
	if _, err := os.Stat(filepath.Join(cache, "ffmpeg-meta.json")); err != nil {
		t.Errorf("缺少元信息文件: %v", err)
	}
}

// 下载内容与期望大小不符(代理返回错误页的典型症状)必须报错,不能落盘。
func TestLocateRejectsWrongSizeDownload(t *testing.T) {
	a := currentAsset(t)
	cache := t.TempDir()
	srv := assetServer(t, a.Size-1024)
	t.Setenv("TVTRIM_PROXY", srv.URL)
	// 清掉 PATH 兜底,确保看到的是下载本身的错误。
	t.Setenv("PATH", filepath.Join(t.TempDir(), "no-bin"))

	_, err := Locate(context.Background(), LocateOptions{CacheDir: cache})
	if err == nil || !strings.Contains(err.Error(), "下载内容大小异常") {
		t.Fatalf("大小不符应报错,实际: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, cacheBinaryName())); err == nil {
		t.Error("校验失败时不应留下成品缓存")
	}
	left, _ := filepath.Glob(filepath.Join(cache, ".download-*"))
	if len(left) != 0 {
		t.Errorf("临时文件应被清理: %v", left)
	}
}

func TestLocateRejectsHTTPError(t *testing.T) {
	cache := t.TempDir()
	srv := assetServer(t, 0) // 500
	t.Setenv("TVTRIM_PROXY", srv.URL)
	t.Setenv("PATH", filepath.Join(t.TempDir(), "no-bin"))

	_, err := Locate(context.Background(), LocateOptions{CacheDir: cache})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("HTTP 错误应被上报,实际: %v", err)
	}
}

// 禁用下载时,PATH 里的 ffmpeg 作为兜底。
func TestLocateNoDownloadFallsBackToPath(t *testing.T) {
	bin := t.TempDir()
	p := filepath.Join(bin, "ffmpeg")
	writeSizedExec(t, p, 8)
	t.Setenv("PATH", bin)

	var msgs []string
	runner, err := Locate(context.Background(), LocateOptions{
		CacheDir: t.TempDir(), NoDownload: true, Progress: reportTo(&msgs),
	})
	if err != nil {
		t.Fatalf("PATH 兜底失败: %v", err)
	}
	if runner.Path != p {
		t.Errorf("路径 = %q, 期望 PATH 中的 %q", runner.Path, p)
	}
	if !strings.Contains(strings.Join(msgs, "\n"), "回退使用 PATH") &&
		!strings.Contains(strings.Join(msgs, "\n"), "禁用下载,使用 PATH") {
		t.Errorf("进度消息不符: %+v", msgs)
	}
}

// 禁用下载且 PATH 里也没有 ffmpeg 时应给出可操作的错误。
func TestLocateNoDownloadWithoutAnyFFmpeg(t *testing.T) {
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))
	_, err := Locate(context.Background(), LocateOptions{CacheDir: t.TempDir(), NoDownload: true})
	if err == nil || !strings.Contains(err.Error(), "未找到可用的 ffmpeg") {
		t.Fatalf("错误提示不符: %v", err)
	}
}

// 下载失败但 PATH 有 ffmpeg 时,回退而不是中断用户。
func TestLocateDownloadFailureFallsBackToPath(t *testing.T) {
	a := currentAsset(t)
	cache := t.TempDir()
	srv := assetServer(t, a.Size-1)
	t.Setenv("TVTRIM_PROXY", srv.URL)

	bin := t.TempDir()
	p := filepath.Join(bin, "ffmpeg")
	writeSizedExec(t, p, 8)
	t.Setenv("PATH", bin)

	runner, err := Locate(context.Background(), LocateOptions{CacheDir: cache})
	if err != nil {
		t.Fatalf("应回退到 PATH: %v", err)
	}
	if runner.Path != p {
		t.Errorf("路径 = %q, 期望回退到 %q", runner.Path, p)
	}
}

func TestCacheUpToDate(t *testing.T) {
	a := currentAsset(t)
	dir := t.TempDir()
	same := filepath.Join(dir, "same")
	writeSizedExec(t, same, a.Size)
	if !cacheUpToDate(same) {
		t.Error("字节数一致应视为最新")
	}
	diff := filepath.Join(dir, "diff")
	writeSizedExec(t, diff, a.Size+1)
	if cacheUpToDate(diff) {
		t.Error("字节数不一致应视为过期")
	}
	if !cacheUpToDate(filepath.Join(dir, "ghost")) {
		t.Error("文件不存在时应放行(按过期处理由调用方兜底)")
	}
}

func TestBuildURL(t *testing.T) {
	raw := rawURL("ffmpeg")
	if raw != "https://raw.githubusercontent.com/totootao/ffmpeg-trim/main/ffmpeg" {
		t.Errorf("rawURL = %q", raw)
	}

	t.Setenv("TVTRIM_PROXY", "")
	if got := buildURL(raw); got != DefaultProxy+"/"+raw {
		t.Errorf("默认代理拼接错误: %q", got)
	}
	t.Setenv("TVTRIM_PROXY", "https://example.com/proxy/")
	if got := buildURL(raw); got != "https://example.com/proxy/"+raw {
		t.Errorf("应去掉结尾多余的斜杠: %q", got)
	}
	for _, none := range []string{"none", "-"} {
		t.Setenv("TVTRIM_PROXY", none)
		if got := buildURL(raw); got != raw {
			t.Errorf("TVTRIM_PROXY=%s 时应直连,实际 %q", none, got)
		}
	}
}

func TestPickAssetAndCacheName(t *testing.T) {
	a, err := pickAsset()
	if err != nil {
		t.Skipf("当前架构无资产: %v", err)
	}
	if a.Size <= 0 {
		t.Errorf("资产大小未登记: %+v", a)
	}
	name := cacheBinaryName()
	if !strings.HasPrefix(name, fmt.Sprintf("ffmpeg-%s-%s", runtime.GOOS, runtime.GOARCH)) {
		t.Errorf("缓存文件名不符: %q", name)
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(name, ".exe") {
		t.Errorf("Windows 下应有 .exe 后缀: %q", name)
	}
}

func TestDefaultCacheDir(t *testing.T) {
	dir := defaultCacheDir()
	if dir == "" {
		t.Fatal("缓存目录不应为空")
	}
	if filepath.Base(dir) != "tvtrim" {
		t.Errorf("缓存目录应叫 tvtrim,实际 %q", dir)
	}
}

func TestWriteMetadata(t *testing.T) {
	dir := t.TempDir()
	writeMetadata(dir, asset{Name: "ffmpeg", GOARCH: runtime.GOARCH}, 1024, "abc123")
	b, err := os.ReadFile(filepath.Join(dir, "ffmpeg-meta.json"))
	if err != nil {
		t.Fatalf("元信息未写入: %v", err)
	}
	s := string(b)
	for _, want := range []string{trimRepo, trimBranch, "ffmpeg", "abc123", "1024"} {
		if !strings.Contains(s, want) {
			t.Errorf("元信息缺少 %q: %s", want, s)
		}
	}
}
