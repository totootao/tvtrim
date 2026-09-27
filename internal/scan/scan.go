// Package scan 负责把用户给的路径展开成待处理的剧集文件列表。
package scan

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/totootao/tvtrim/internal/ffmpeg"
)

// Item 是扫描出的一个待处理文件。
type Item struct {
	Path string
	Size int64
	// Season, Episode 是从文件名里解析出的季/集编号(解析不到为 0)。
	Season  int
	Episode int
	// Show 是从文件名里解析出的剧名(解析不到为空,此时分组回退到目录名)。
	// 一个目录里往往混排多部剧,靠这个字段才能把它们拆开处理。
	Show string
}

// Options 控制扫描行为。
type Options struct {
	// Recursive 表示递归子目录。
	Recursive bool
	// ExcludeSuffix 是要排除的文件名后缀(例如 "-trim"),避免把上次的输出再剪一遍。
	ExcludeSuffix string
	// Exts 限定扩展名;为空则用默认白名单。
	Exts []string
}

// Collect 把输入的路径展开成文件列表。
// 传入目录时按扩展名筛选;传入文件时无条件接受(由调用方决定是否校验)。
func Collect(inputs []string, opts Options) ([]Item, error) {
	exts := opts.Exts
	if len(exts) == 0 {
		exts = ffmpeg.SupportedExts
	}
	allow := make(map[string]bool, len(exts))
	for _, e := range exts {
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		allow[strings.ToLower(e)] = true
	}

	var items []Item
	seen := map[string]bool{}

	addFile := func(path string, force bool) error {
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if seen[abs] {
			return nil
		}
		st, err := os.Stat(abs)
		if err != nil {
			return fmt.Errorf("无法读取 %s: %w", path, err)
		}
		if st.IsDir() {
			return nil
		}

		if !force {
			ext := strings.ToLower(filepath.Ext(abs))
			if !allow[ext] {
				return nil
			}
		}
		// 排除上次生成的输出文件,防止重复裁剪。
		if opts.ExcludeSuffix != "" && strings.Contains(
			strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs)),
			opts.ExcludeSuffix) {
			return nil
		}
		// 跳过隐藏文件和临时文件。
		base := filepath.Base(abs)
		if strings.HasPrefix(base, ".") || strings.HasSuffix(base, ".part") {
			return nil
		}

		seen[abs] = true
		season, episode := ParseEpisode(base)
		items = append(items, Item{
			Path: abs, Size: st.Size(), Season: season, Episode: episode,
			Show: ShowOf(base),
		})
		return nil
	}

	for _, in := range inputs {
		st, err := os.Stat(in)
		if err != nil {
			return nil, fmt.Errorf("路径不存在: %s", in)
		}
		if !st.IsDir() {
			// 显式指定的文件不做扩展名过滤,交给 ffmpeg 自己判断。
			if err := addFile(in, true); err != nil {
				return nil, err
			}
			continue
		}

		if opts.Recursive {
			err = filepath.WalkDir(in, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return nil // 忽略无权限的子目录,继续扫描
				}
				if d.IsDir() {
					if strings.HasPrefix(d.Name(), ".") && path != in {
						return filepath.SkipDir
					}
					return nil
				}
				return addFile(path, false)
			})
		} else {
			var entries []os.DirEntry
			entries, err = os.ReadDir(in)
			if err != nil {
				return nil, fmt.Errorf("读取目录失败 %s: %w", in, err)
			}
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				if err := addFile(filepath.Join(in, e.Name()), false); err != nil {
					return nil, err
				}
			}
		}
		if err != nil {
			return nil, err
		}
	}

	// 排序规则:有编号的剧集按 季→集 升序排在最前;完全解析不出编号的文件统一排在最后,
	// 再按路径字典序保证稳定。这样 Web 界面与 CLI 输出的顺序一致,便于人工核对。
	numbered := func(it Item) int {
		if it.Season == 0 && it.Episode == 0 {
			return 1
		}
		return 0
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if na, nb := numbered(a), numbered(b); na != nb {
			return na < nb
		}
		if a.Season != b.Season {
			return a.Season < b.Season
		}
		if a.Episode != b.Episode {
			return a.Episode < b.Episode
		}
		return a.Path < b.Path
	})

	return items, nil
}

// ParseEpisode 从文件名中解析季/集编号。
// 支持 S01E02、第01集、EP02、E02 等常见写法,解析不到返回 (0,0)。
func ParseEpisode(name string) (season, episode int) {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	lower := strings.ToLower(base)

	// S01E02 / s1e2
	if s, e, _, ok := findSE(lower); ok {
		return s, e
	}

	// 第01集 / 第1话
	if e, _, ok := findChineseEp(base); ok {
		return 0, e
	}

	// EP02 / E02 / 第02集
	if e, _, ok := findPrefixEp(lower); ok {
		return 0, e
	}
	return 0, 0
}

// findSE 解析 SxxExx 形式,同时返回标记的起始下标(用于截取剧名)。
func findSE(s string) (season, episode, idx int, ok bool) {
	for i := 0; i+1 < len(s); i++ {
		if s[i] != 's' {
			continue
		}
		if i > 0 && isDigit(s[i-1]) {
			continue // 避免匹配 resource / series 里的 s
		}
		j := i + 1
		for j < len(s) && isDigit(s[j]) {
			j++
		}
		if j == i+1 || j >= len(s) || s[j] != 'e' {
			continue
		}
		k := j + 1
		for k < len(s) && isDigit(s[k]) {
			k++
		}
		if k == j+1 {
			continue
		}
		return atoi(s[i+1 : j]), atoi(s[j+1 : k]), i, true
	}
	return 0, 0, -1, false
}

// matchSE 解析 SxxExx 形式。
func matchSE(s string) (int, int, bool) {
	s1, e1, _, ok := findSE(s)
	return s1, e1, ok
}

// findChineseEp 解析 "第N集/话/期/部" 形式,返回剧集号与"第"的下标。
func findChineseEp(s string) (episode, idx int, ok bool) {
	for _, marker := range []string{"第"} {
		idx := strings.Index(s, marker)
		for idx >= 0 {
			j := idx + len(marker)
			k := j
			for k < len(s) && isDigit(s[k]) {
				k++
			}
			if k > j && k < len(s) {
				next := string([]rune(s[k:])[0])
				if next == "集" || next == "话" || next == "期" || next == "部" {
					return atoi(s[j:k]), idx, true
				}
			}
			next := strings.Index(s[idx+1:], marker)
			if next < 0 {
				break
			}
			idx = idx + 1 + next
		}
	}
	return 0, -1, false
}

// matchChineseEp 解析 "第N集/话/期/部" 形式。
func matchChineseEp(s string) (int, bool) {
	e, _, ok := findChineseEp(s)
	return e, ok
}

// findPrefixEp 解析 "EP02"/"E02" 形式,返回集号与标记下标。
func findPrefixEp(s string) (episode, idx int, ok bool) {
	for _, p := range []string{"ep", "e"} {
		idx := strings.Index(s, p)
		if idx < 0 {
			continue
		}
		// 必须处于词首(前面不是字母数字)。
		if idx > 0 {
			c := s[idx-1]
			if isDigit(c) || (c >= 'a' && c <= 'z') {
				continue
			}
		}
		j := idx + len(p)
		k := j
		for k < len(s) && isDigit(s[k]) {
			k++
		}
		if k > j {
			return atoi(s[j:k]), idx, true
		}
	}
	return 0, -1, false
}

// matchPrefixEp 解析 "EP02"/"E02" 形式。
func matchPrefixEp(s string) (int, bool) {
	e, _, ok := findPrefixEp(s)
	return e, ok
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}
