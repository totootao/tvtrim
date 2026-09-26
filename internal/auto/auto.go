// Package auto 基于 silencedetect 静音区间自动识别片头曲与片尾曲的边界。
//
// 典型剧集的音频结构:
//
//	[黑屏静音] [片头曲 OP] [静音切点] [正片] [静音切点] [片尾曲 ED] [静音→末尾]
//
// 识别策略:
//   - 片头:在 OP 合理时长窗口内找第一个足够长的静音,其结束点即正片起点
//   - 片尾:先定位"延伸到文件末尾的长静音"(ED 结束),再在它之前找
//     "正片→ED 的切点静音",若两者间距落在 ED 合理长度内则切点成立
//
// 同一部剧每集的 OP/ED 时长完全相同,因此多集检测结果可做众数纠错,
// 个别集的误判(正片内对白长停顿等)会被多数集的值修正。
package auto

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/totootao/tvtrim/internal/ffmpeg"
)

// 可调参数(秒)。
const (
	// NoiseDB 是静音判定阈值(dB,负值):低于该响度视为静音。
	// 转场黑屏通常是数字静音(-90dB),对白停顿在 -35dB 下不会被误判。
	NoiseDB = -35.0
	// MinSilence 参与切点判定的最短静音时长。对白长停顿通常 < 1s,
	// 转场静音(黑屏)普遍 1s 以上。
	MinSilence = 0.8
	// HeadWinMin/HeadWinMax 是片头切点的搜索窗口(从文件开头算)。
	// OP 结束点几乎总是落在 90s±30s,窗口放宽到 [20, 210] 覆盖特例。
	HeadWinMin = 20.0
	HeadWinMax = 210.0
	// TailWinBack 从文件末尾往前搜索片尾区域的时间跨度。
	TailWinBack = 420.0
	// EDMinLen/EDMaxLen 是"正片切点→ED 结束"的合理长度范围,
	// 用来排除把正片内部静音误认成片尾切点的情况。
	// 多集众数纠错会兜住残余误判,下限不必设得太苛刻。
	EDMinLen = 10.0
	EDMaxLen = 330.0
)

// Result 是单个文件的自动识别结果(单位:秒)。
type Result struct {
	// Head 是从文件开头到正片起点的时长(即 -head 参数值)。
	Head float64
	// Tail 是从正片结束点到文件末尾的时长(即 -tail 参数值)。
	Tail float64
	// Fixed 表示该结果在众数纠错中被修正过。
	Fixed bool
	// Note 记录判定依据或修正说明,用于展示。
	Note string
	// OK 为 false 表示识别失败(需要手动指定)。
	OK bool
}

// Detect 基于静音区间识别单个文件的片头片尾。
// total 是文件总时长(秒),sils 是 silencedetect 的检测结果。
func Detect(total float64, sils []ffmpeg.SilenceRange) (Result, error) {
	if total <= 0 {
		return Result{}, errors.New("文件总时长未知")
	}
	if len(sils) == 0 {
		return Result{}, errors.New("未检测到任何静音区间,无法定位切点")
	}

	head, err := detectHead(sils)
	if err != nil {
		return Result{}, err
	}

	tail, note := detectTail(total, head, sils)
	res := Result{Head: head, Tail: tail, OK: true, Note: note}
	if tail == 0 {
		res.Note = "已识别片头;" + note
	}
	return res, nil
}

// detectHead 在 OP 窗口内找第一个足够长的静音,其结束点即正片起点。
func detectHead(sils []ffmpeg.SilenceRange) (float64, error) {
	for _, s := range sils {
		if s.Duration < MinSilence {
			continue
		}
		if s.End >= HeadWinMin && s.End <= HeadWinMax {
			return s.End, nil
		}
		if s.End > HeadWinMax {
			break
		}
	}
	return 0, fmt.Errorf("在 %.0f~%.0f 秒内未找到 ≥%.1fs 的静音切点(片头曲结束处)",
		HeadWinMin, HeadWinMax, MinSilence)
}

// detectTail 定位片尾区域:
//  1. 找"延伸到文件末尾"的长静音(ED 结束后的黑屏/静音),其 Start 即 ED 结束点;
//  2. 在 ED 结束点之前找最后一个切点静音(正片→ED 的转场),
//     若间距合理,tail = total - 该静音的 End(ED 被完整剪掉);
//  3. 若找不到切点(正片与 ED 硬切),退而只剪 ED 之后的部分。
func detectTail(total, head float64, sils []ffmpeg.SilenceRange) (float64, string) {
	tailWinStart := total - TailWinBack
	if tailWinStart < head {
		tailWinStart = head
	}

	// 1) ED 结束点:末尾窗口内最后一个长静音,且其结束接近文件末尾。
	var edEndSil *ffmpeg.SilenceRange
	for i := len(sils) - 1; i >= 0; i-- {
		s := sils[i]
		if s.Duration < MinSilence || s.Start < tailWinStart || s.Start <= head {
			continue
		}
		if s.End >= total-3 { // 延伸到末尾(容差 3s)
			edEndSil = &sils[i]
			break
		}
		// 静音在末尾窗口内但不贴着文件尾(如 ED 后还有预告):取最后一个仍可作参考。
		if edEndSil == nil {
			edEndSil = &sils[i]
		}
	}
	if edEndSil == nil {
		return 0, "末尾区域未找到长静音,无法定位片尾"
	}
	edFinish := edEndSil.Start // ED 结束、黑屏静音开始处

	// 2) 正片→ED 切点:edFinish 之前最后一个长静音。
	var midSil *ffmpeg.SilenceRange
	for i := len(sils) - 1; i >= 0; i-- {
		s := sils[i]
		if s.End <= head {
			break
		}
		if s.Duration < MinSilence || s.End >= edFinish {
			continue
		}
		if s.End > head {
			midSil = &sils[i]
			break
		}
	}

	if midSil != nil {
		// 切在静音 Start(正片音频结束处):转场静音与 ED 一并剪除,
		// 输出末尾不残留黑屏/预告画面。
		edLen := edFinish - midSil.Start
		if edLen >= EDMinLen && edLen <= EDMaxLen {
			tail := total - midSil.Start
			return tail, fmt.Sprintf("片头切点 %.1fs,片尾切点 %.1fs(ED %.0fs)", head, midSil.Start, edLen)
		}
		return total - edFinish,
			fmt.Sprintf("正片→ED 无静音切点,仅剪除 ED 之后 %.1fs", total-edFinish)
	}
	return total - edFinish,
		fmt.Sprintf("未找到 ED 起点切点,仅剪除 ED 之后 %.1fs", total-edFinish)
}

// ModeTolerance 是众数纠错的容差(秒):检测结果相互偏离小于该值视为一致。
// 关键帧对齐会带来 ±1~2s 的差异,1.5s 是比较稳的分界。
const ModeTolerance = 1.5

// Correct 对多集识别结果做众数纠错:
// 分别对 Head/Tail 聚类,取最大簇(容差内互为一致)的均值作为众数值,
// 偏离众数超过容差的集用众数替换并标记 Fixed。
// 单集或全部失败时不做任何事。
func Correct(rs []Result) []Result {
	if len(rs) < 2 {
		return rs
	}

	// 收集参与众数统计的值;Tail=0 表示"未识别到片尾",不参与。
	var headVals, tailVals []float64
	for _, r := range rs {
		if !r.OK {
			continue
		}
		headVals = append(headVals, r.Head)
		if r.Tail > 0 {
			tailVals = append(tailVals, r.Tail)
		}
	}
	heads := modeOf(headVals, ModeTolerance)
	tails := modeOf(tailVals, ModeTolerance)

	out := make([]Result, len(rs))
	copy(out, rs)
	for i := range out {
		if !out[i].OK {
			continue
		}
		var fixes []string
		if heads.ok && math.Abs(out[i].Head-heads.value) > ModeTolerance {
			fixes = append(fixes, fmt.Sprintf("片头 %.1fs→%.1fs", out[i].Head, heads.value))
			out[i].Head = heads.value
			out[i].Fixed = true
		}
		if tails.ok && out[i].Tail > 0 && math.Abs(out[i].Tail-tails.value) > ModeTolerance {
			fixes = append(fixes, fmt.Sprintf("片尾 %.1fs→%.1fs", out[i].Tail, tails.value))
			out[i].Tail = tails.value
			out[i].Fixed = true
		}
		if len(fixes) > 0 {
			out[i].Note = "按多数集修正: " + join(fixes, "; ")
		}
	}
	return out
}

// Agreement 返回与第 i 个结果(已纠错)一致的集数,用于展示置信度。
// 一致 = 该维度与结果值相差不超过容差。
func Agreement(rs []Result, i int) (agree, total int) {
	if i < 0 || i >= len(rs) || !rs[i].OK {
		return 0, len(rs)
	}
	for _, r := range rs {
		if !r.OK {
			continue
		}
		if math.Abs(r.Head-rs[i].Head) <= ModeTolerance &&
			math.Abs(r.Tail-rs[i].Tail) <= ModeTolerance {
			agree++
		}
	}
	return agree, len(rs)
}

// modeStat 是一次众数计算的结果。
type modeStat struct {
	value float64
	ok    bool
}

// modeOf 在数值集合中找最大容差簇的均值。
func modeOf(vals []float64, tol float64) modeStat {
	if len(vals) == 0 {
		return modeStat{}
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)

	bestLo, bestHi, bestCnt := 0, 0, 0
	lo := 0
	for hi := 0; hi < len(sorted); hi++ {
		for sorted[hi]-sorted[lo] > 2*tol {
			lo++
		}
		if hi-lo+1 > bestCnt {
			bestLo, bestHi, bestCnt = lo, hi, hi-lo+1
		}
	}
	if bestCnt < 2 {
		return modeStat{} // 没有多数派,不纠错
	}
	sum := 0.0
	for i := bestLo; i <= bestHi; i++ {
		sum += sorted[i]
	}
	return modeStat{value: sum / float64(bestCnt), ok: true}
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
