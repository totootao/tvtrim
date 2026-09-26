package ffmpeg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// 默认的 GitHub 加速代理。可通过 TVTRIM_PROXY 环境变量覆盖;
// 设为 "none" 表示不走代理直连。
const DefaultProxy = "https://ghproxy.totootao.top"

// ffmpeg-trim 的发布源。
const (
	trimRepo   = "totootao/ffmpeg-trim"
	trimBranch = "main"
)

// asset 描述 ffmpeg-trim 仓库里的一个二进制资产。
type asset struct {
	Name   string // 仓库中的文件名
	GOARCH string // 对应的 Go 架构标识
	Size   int64  // 期望字节数,用于下载校验
}

// 实测得到的资产清单(来自 ffmpeg-trim 仓库)。
// v2 构建新增了静音检测滤镜与更多容器/解码器,体积略增。
var assets = []asset{
	{Name: "ffmpeg", GOARCH: "amd64", Size: 2994224},
	{Name: "ffmpeg-aarch64", GOARCH: "arm64", Size: 2690944},
}

// LocateOptions 控制 ffmpeg 二进制的定位行为。
type LocateOptions struct {
	// Explicit 是用户通过 --ffmpeg 显式指定的路径,优先级最高。
	Explicit string
	// CacheDir 是自动下载的缓存目录,默认为用户缓存目录下的 tvtrim。
	CacheDir string
	// NoDownload 为 true 时禁止自动下载,仅使用已有文件。
	NoDownload bool
	// Progress 用于输出下载进度,可为 nil。
	Progress func(msg string)
}

// Locate 按优先级查找可用的 ffmpeg:
//  1. Explicit(--ffmpeg 参数)
//  2. TVTRIM_FFMPEG 环境变量
//  3. 缓存目录中已下载的 ffmpeg-trim
//  4. 从 ffmpeg-trim 仓库自动下载
//  5. PATH 中的 ffmpeg(仅当禁用下载或下载失败时兜底)
//
// 注意 PATH 排在自动下载之后:项目指定的后端是 ffmpeg-trim,
// PATH 里可能是带编码器的完整版 ffmpeg —— 参数语义并不完全一致
// (例如完整版会接受输入侧 -ss,trim 版会静默忽略),优先用 trim 版更可预期。
func Locate(ctx context.Context, opts LocateOptions) (*Runner, error) {
	report := opts.Progress
	if report == nil {
		report = func(string) {}
	}

	// 1. 显式指定
	if opts.Explicit != "" {
		abs, err := filepath.Abs(opts.Explicit)
		if err != nil {
			return nil, err
		}
		if err := checkExecutable(abs); err != nil {
			return nil, fmt.Errorf("--ffmpeg 指定的文件不可用: %w", err)
		}
		return &Runner{Path: abs}, nil
	}

	// 2. 环境变量
	if env := os.Getenv("TVTRIM_FFMPEG"); env != "" {
		if err := checkExecutable(env); err != nil {
			return nil, fmt.Errorf("TVTRIM_FFMPEG 指定的文件不可用: %w", err)
		}
		return &Runner{Path: env}, nil
	}

	// 3. 缓存
	cacheDir := opts.CacheDir
	if cacheDir == "" {
		cacheDir = defaultCacheDir()
	}
	cached := filepath.Join(cacheDir, cacheBinaryName())
	if checkExecutable(cached) == nil {
		report(fmt.Sprintf("使用缓存的 ffmpeg-trim: %s", cached))
		return &Runner{Path: cached}, nil
	}

	// 5 的准备:PATH 兜底候选(下载失败或禁用下载时使用)。
	pathFallback, pathErr := lookTrimInPath()

	if opts.NoDownload {
		if pathErr == nil {
			report(fmt.Sprintf("已禁用下载,使用 PATH 中的 ffmpeg: %s", pathFallback))
			return &Runner{Path: pathFallback}, nil
		}
		return nil, fmt.Errorf("未找到可用的 ffmpeg,且已禁用自动下载。请用 --ffmpeg 指定路径")
	}

	// 4. 自动下载
	report("本地未找到 ffmpeg-trim,开始自动下载…")
	path, err := download(ctx, cacheDir, report)
	if err != nil {
		// 下载失败回退 PATH,并附带下载失败原因。
		if pathErr == nil {
			report(fmt.Sprintf("自动下载失败(%v),回退使用 PATH 中的 ffmpeg: %s", err, pathFallback))
			return &Runner{Path: pathFallback}, nil
		}
		return nil, err
	}
	return &Runner{Path: path}, nil
}

// checkExecutable 校验路径存在、是普通文件且具备可执行权限。
func checkExecutable(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("%s 是目录", path)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s 没有可执行权限,请先 chmod +x", path)
	}
	return nil
}

// lookTrimInPath 在 PATH 中查找 ffmpeg。
func lookTrimInPath() (string, error) {
	return exec.LookPath("ffmpeg")
}

// cacheBinaryName 是缓存目录中二进制使用的文件名(带平台后缀,便于多平台共存)。
func cacheBinaryName() string {
	name := fmt.Sprintf("ffmpeg-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// defaultCacheDir 返回默认缓存目录。
func defaultCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "tvtrim")
}

// pickAsset 根据当前架构挑选对应的 ffmpeg-trim 资产。
func pickAsset() (asset, error) {
	for _, a := range assets {
		if a.GOARCH == runtime.GOARCH {
			return a, nil
		}
	}
	return asset{}, fmt.Errorf("ffmpeg-trim 未提供 %s/%s 平台的构建,请用 --ffmpeg 手动指定",
		runtime.GOOS, runtime.GOARCH)
}

// buildURL 组装资产的下载地址,按需套用代理。
func buildURL(rawURL string) string {
	proxy := os.Getenv("TVTRIM_PROXY")
	if proxy == "" {
		proxy = DefaultProxy
	}
	if proxy == "none" || proxy == "-" {
		return rawURL
	}
	return strings.TrimRight(proxy, "/") + "/" + rawURL
}

// rawURL 返回资产在 GitHub raw 上的原始地址。
func rawURL(name string) string {
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", trimRepo, trimBranch, name)
}

// download 下载 ffmpeg-trim 到 cacheDir,校验大小后原子落盘。
func download(ctx context.Context, cacheDir string, report func(string)) (string, error) {
	a, err := pickAsset()
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("创建缓存目录失败: %w", err)
	}

	url := buildURL(rawURL(a.Name))
	report(fmt.Sprintf("下载 %s (%s/%s, 约 %.1f MB)…", url, runtime.GOOS, runtime.GOARCH, float64(a.Size)/1024/1024))

	// 先下到同目录的临时文件,校验通过后再改名,避免半截文件被当成可用缓存。
	tmp, err := os.CreateTemp(cacheDir, ".download-*")
	if err != nil {
		return "", fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath) // 成功后该文件已被 rename,此处无害
	}()

	client := &http.Client{Timeout: 10 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "tvtrim/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载失败: %w(可用 TVTRIM_PROXY=none 直连,或用 --ffmpeg 手动指定)", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载失败: HTTP %d(可用 TVTRIM_PROXY 换代理,或用 --ffmpeg 手动指定)", resp.StatusCode)
	}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if err != nil {
		return "", fmt.Errorf("下载中断: %w", err)
	}

	// 大小校验:防止代理返回了错误页或半截内容。
	// Size 为 0 表示未知(如新架构刚发布),跳过该校验。
	if a.Size > 0 && n != a.Size {
		return "", fmt.Errorf("下载内容大小异常: 期望 %d 字节,实际 %d 字节(代理可能返回了错误内容)", a.Size, n)
	}

	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	dst := filepath.Join(cacheDir, cacheBinaryName())
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return "", fmt.Errorf("写入缓存失败: %w", err)
	}

	sum := hex.EncodeToString(h.Sum(nil))
	writeMetadata(cacheDir, a, n, sum)
	report(fmt.Sprintf("下载完成: %s (sha256 %s)", dst, sum[:16]))
	return dst, nil
}

// Metadata 是缓存的元信息,记录二进制来源与哈希,便于排查。
type Metadata struct {
	Repo      string    `json:"repo"`
	Branch    string    `json:"branch"`
	Asset     string    `json:"asset"`
	GOOS      string    `json:"goos"`
	GOARCH    string    `json:"goarch"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	FetchedAt time.Time `json:"fetched_at"`
}

// writeMetadata 把下载元信息写到缓存目录,失败不影响主流程。
func writeMetadata(cacheDir string, a asset, size int64, sum string) {
	md := Metadata{
		Repo: trimRepo, Branch: trimBranch, Asset: a.Name,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		Size: size, SHA256: sum, FetchedAt: time.Now(),
	}
	b, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(cacheDir, "ffmpeg-meta.json"), b, 0o644)
}
