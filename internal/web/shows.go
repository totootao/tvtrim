// Package web 提供一个本地 Web 界面,用于预览扫描/识别结果并人工确认执行。
//
// 设计约束:不引入任何第三方依赖,只用标准库 net/http + encoding/json,
// 前端是单个 HTML 文件(go:embed),没有任何外部资源请求,离线可用。
package web

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/totootao/tvtrim/internal/auto"
	"github.com/totootao/tvtrim/internal/ffmpeg"
	"github.com/totootao/tvtrim/internal/scan"
)

// Item 是待处理的一集,携带展示与确认所需的全部信息。
type Item struct {
	Path      string  `json:"path"`
	Name      string  `json:"name"`
	Season    int     `json:"season"`
	Episode   int     `json:"episode"`
	Size      int64   `json:"size"`
	Duration  float64 `json:"duration"`
	Container string  `json:"container"`
	// Head/Tail 是建议砍掉的头尾秒数(-auto 识别结果或用户手动时长)。
	Head float64 `json:"head"`
	Tail float64 `json:"tail"`
	// Ready 为 false 表示切点不可用(识别失败),不能参与执行。
	Ready bool   `json:"ready"`
	Note  string `json:"note"`
}

// Show 表示一部剧(或一个季)下的多集。
// 分组维度是 (文件所在目录, 季号),这样同一目录下混排多季也能分开确认。
type Show struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Season  int    `json:"season"`
	Dir     string `json:"dir"`
	Items   []Item `json:"items"`
	Ready   int    `json:"ready"` // 可执行的集数
	TotalIn int64  `json:"total_in"`
}

// BuildShows 把扫描结果与探测/识别结果组装成分组视图。
//
// proberes 提供每集的时长与容器信息;res 提供 -auto 识别的切点,
// 手动模式下 res 为 nil,由 head/tailSec 提供统一值。
func BuildShows(items []scan.Item, probes map[string]*ffmpeg.ProbeResult,
	res map[string]auto.Result, headSec, tailSec float64) []Show {

	byKey := map[string]*Show{}
	var order []string

	for _, it := range items {
		dir := filepath.Dir(it.Path)
		season := it.Season
		key := fmt.Sprintf("%s|S%02d", dir, season)
		sh, ok := byKey[key]
		if !ok {
			name := filepath.Base(dir)
			if name == "" || name == "." || name == string(filepath.Separator) {
				name = dir
			}
			if season > 0 {
				name = fmt.Sprintf("%s  S%02d", name, season)
			}
			sh = &Show{ID: key, Name: name, Season: season, Dir: dir}
			byKey[key] = sh
			order = append(order, key)
		}

		item := Item{
			Path:    it.Path,
			Name:    filepath.Base(it.Path),
			Season:  it.Season,
			Episode: it.Episode,
			Size:    it.Size,
			Head:    headSec,
			Tail:    tailSec,
			Ready:   headSec > 0 || tailSec > 0,
			Note:    "手动指定",
		}
		if p, ok := probes[it.Path]; ok && p != nil {
			item.Duration = p.DurationSeconds()
			item.Container = p.Container
		}
		if res != nil {
			if r, ok := res[it.Path]; ok {
				item.Head, item.Tail = r.Head, r.Tail
				item.Ready = r.OK
				if r.OK {
					item.Note = r.Note
				} else {
					item.Note = "识别失败:" + r.Note
				}
			} else {
				item.Ready = false
				item.Note = "未识别"
			}
		}
		sh.TotalIn += it.Size
		if item.Ready {
			sh.Ready++
		}
		sh.Items = append(sh.Items, item)
	}

	// 排序规则:有季号的分组在前,全部没季号的分组(通常是花絮/预告等零散文件)在后,
	// 组内再按 ID(目录|季)字典序,保证多次刷新顺序稳定。
	sort.SliceStable(order, func(i, j int) bool {
		a, b := byKey[order[i]], byKey[order[j]]
		if (a.Season == 0) != (b.Season == 0) {
			return b.Season == 0
		}
		return order[i] < order[j]
	})
	shows := make([]Show, 0, len(order))
	for _, k := range order {
		sh := byKey[k]
		sort.SliceStable(sh.Items, func(i, j int) bool {
			a, b := sh.Items[i], sh.Items[j]
			if a.Episode != b.Episode {
				// 无编号的排在最后,避免 S/E 未识别时的抖动。
				if a.Episode == 0 {
					return false
				}
				if b.Episode == 0 {
					return true
				}
				return a.Episode < b.Episode
			}
			return a.Name < b.Name
		})
		shows = append(shows, *sh)
	}
	return shows
}

// Summary 是 show 列表的聚合信息,用于页面顶部展示。
type Summary struct {
	Shows   int     `json:"shows"`
	Items   int     `json:"items"`
	Ready   int     `json:"ready"`
	TotalIn int64   `json:"total_in"`
	DurSum  float64 `json:"duration_sum"`
}

// Summarize 汇总 show 列表。
func Summarize(shows []Show) Summary {
	var s Summary
	for _, sh := range shows {
		s.Shows++
		for _, it := range sh.Items {
			s.Items++
			s.DurSum += it.Duration
			if it.Ready {
				s.Ready++
			}
		}
		s.TotalIn += sh.TotalIn
	}
	return s
}

// HumanSize 把字节数格式化成可读单位(供页面使用)。
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	exp := 0
	v := float64(n)
	for v >= unit && exp < 5 {
		v /= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", v, "KMGTP"[exp-1])
}

// TrimNote 去掉 Note 里过长的细节,供窄列展示。
func TrimNote(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
