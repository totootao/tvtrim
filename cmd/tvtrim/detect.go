package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/totootao/tvtrim/internal/auto"
	"github.com/totootao/tvtrim/internal/ffmpeg"
	"github.com/totootao/tvtrim/internal/scan"
	"github.com/totootao/tvtrim/internal/trim"
)

// detectResult 是单个文件的探测与自动识别结果。
type detectResult struct {
	Item  scan.Item
	Probe *ffmpeg.ProbeResult
	Res   auto.Result
	// Err 是探测/静音检测/识别任一环节的失败原因,非空时 Res 无意义。
	Err error
}

// OK 表示该文件的切点已成功识别。
func (d detectResult) OK() bool { return d.Err == nil && d.Res.OK }

// DetectMode 描述识别阶段的输入来源。
type DetectMode int

const (
	// DetectSilence 用静音检测自动识别每集切点(-auto)。
	DetectSilence DetectMode = iota
	// DetectProbeOnly 只探测时长,不识别切点。
	DetectProbeOnly
)

// DefaultSamplePerShow 是 -sample 的默认值:每部剧抽 3 集做静音检测。
//
// 同一部剧的 OP/ED 时长全剧一致,没必要每集都整片解码音频。
// 取 3 是因为它能容一集误判(3 集里 2 集一致即可判定)——2 集互不相同时
// 无法判断谁可信,代码会退回全量重测。
const DefaultSamplePerShow = 3

// detectConfig 是 detectAll 的配置。
type detectConfig struct {
	// Mode 决定是否在探测之外做静音检测。
	Mode DetectMode
	// Workers 是并发数,<=0 时取 CPU 核数。
	Workers int
	// Sample 是每部剧参与静音检测的集数,<=0 表示全部都测。
	Sample int
	// Out 是进度输出目标,nil 表示不显示进度。
	Out io.Writer
}

// detectAll 返回按文件路径排序的结果,流程分三步:
//
//	步骤一 并发探测全部文件(只读元数据,快;裁剪算终点必须有总时长)
//	步骤二 只对抽样集做静音检测(整片解码音频,是耗时大头)
//	步骤三 众数纠错 → 把每部剧的切点推广到未抽样的集
//
// Mode 为 DetectProbeOnly 时只做步骤一。
func detectAll(ctx context.Context, runner *ffmpeg.Runner, items []scan.Item, cfg detectConfig) []detectResult {
	if len(items) == 0 {
		return nil
	}
	workers := effectiveWorkers(cfg.Workers, len(items))

	// 步骤一:全部文件都要探测。
	probes := probeAll(ctx, runner, items, workers, cfg.Out)
	if cfg.Mode == DetectProbeOnly {
		out := make([]detectResult, 0, len(items))
		for _, it := range items {
			out = append(out, detectResult{Item: it, Probe: probes[it.Path]})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Item.Path < out[j].Item.Path })
		return out
	}

	// 步骤二:只抽样一部分做静音检测。
	targets := pickSilenceTargets(items, cfg.Sample)
	res := silenceDetect(ctx, runner, items, probes, workers, cfg.Out, targets)

	// 抽样集互相矛盾时该剧结论不可信,退回整部剧重测。
	// sample<=1 时没有第二个样本可比,重测也没意义,尊重用户的显式选择。
	if extra := expandInconsistentGroups(items, res, cfg.Sample); extra != nil {
		fprintln(cfg.Out, fmt.Sprintf(
			"抽样集结论不一致,改用全部 %d 集重新判定(可用 -sample 关闭抽样)", len(extra)))
		for path, r := range silenceDetect(ctx, runner, items, probes, workers, cfg.Out, extra) {
			res[path] = r
		}
	}

	// 组装成按路径排序的结果切片(后续按剧团购分组,需要稳定顺序)。
	ordered := make([]detectResult, 0, len(items))
	for _, it := range items {
		d := detectResult{Item: it, Probe: probes[it.Path]}
		if r, ok := res[it.Path]; ok {
			d.Res = r
		}
		ordered = append(ordered, d)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Item.Path < ordered[j].Item.Path })

	// 步骤三。
	applyModeCorrection(ordered)
	broadcastShowValues(ordered)
	refreshAgreementNotes(ordered)
	return ordered
}

// probeAll 并发探测每个文件的元数据;失败的文件不在返回的 map 里。
func probeAll(ctx context.Context, runner *ffmpeg.Runner, items []scan.Item,
	workers int, out io.Writer) map[string]*ffmpeg.ProbeResult {

	res := make(map[string]*ffmpeg.ProbeResult, len(items))
	var mu sync.Mutex
	forEachWithProgress(items, workers, out, func(it scan.Item) {
		p, err := runner.Probe(ctx, it.Path)
		if err != nil {
			return
		}
		mu.Lock()
		res[it.Path] = p
		mu.Unlock()
	})
	return res
}

// silenceDetect 对 targets 中的文件跑静音检测并识别切点。
//
// 返回的 map 只包含识别成功的项 —— 调用方据此区分
// "识别失败" 与 "压根没抽样"(后者会在推广阶段拿到本剧的结论)。
func silenceDetect(ctx context.Context, runner *ffmpeg.Runner, items []scan.Item,
	probes map[string]*ffmpeg.ProbeResult, workers int, out io.Writer,
	targets map[string]bool) map[string]auto.Result {

	var queue []scan.Item
	for _, it := range items {
		if targets[it.Path] {
			queue = append(queue, it)
		}
	}
	res := make(map[string]auto.Result, len(queue))
	if len(queue) == 0 {
		return res
	}

	var mu sync.Mutex
	forEachWithProgress(queue, workers, out, func(it scan.Item) {
		p := probes[it.Path]
		if p == nil { // 探测都没成功,静音检测必然也失败
			return
		}
		total := p.Duration.Seconds()
		sils, err := runner.DetectSilences(ctx, it.Path, auto.NoiseDB, auto.MinSilence, total)
		if err != nil {
			return
		}
		r, err := auto.Detect(total, sils)
		if err != nil || !r.OK {
			return
		}
		mu.Lock()
		res[it.Path] = r
		mu.Unlock()
	})
	return res
}

// forEachWithProgress 按 workers 并发遍历 items,完成后更新进度显示。
func forEachWithProgress(items []scan.Item, workers int, out io.Writer, fn func(scan.Item)) {
	if len(items) == 0 {
		return
	}
	workers = effectiveWorkers(workers, len(items))
	var p *progress
	if out != nil {
		p = newProgress(out, len(items))
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for _, it := range items {
		wg.Add(1)
		go func(it scan.Item) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fn(it)
			if p != nil {
				p.step(filepath.Base(it.Path), true)
			}
		}(it)
	}
	wg.Wait()
	if p != nil {
		p.finish()
	}
}

// pickSilenceTargets 选出真正需要静音检测的文件。
//
// sample<=0 时全部都测;否则每部剧(每一季)只取季集号最小的前 sample 集。
// 分组维度与纠错一致,所以不会出现在前面抽满名额、排在后面的剧一集没测的情况。
func pickSilenceTargets(items []scan.Item, sample int) map[string]bool {
	m := make(map[string]bool, len(items))
	if sample <= 0 {
		for _, it := range items {
			m[it.Path] = true
		}
		return m
	}
	for _, g := range groupByShow(items) {
		n := sample
		if n > len(g.items) {
			n = len(g.items)
		}
		for _, it := range g.items[:n] {
			m[it.Path] = true
		}
	}
	return m
}

// showGroup 是一部剧(的某一季)及其全部文件。
type showGroup struct {
	key   string
	items []scan.Item
}

// groupByShow 按 (目录, 剧名, 季) 分组,组内按"季→集"升序。
// 没有集号的文件排在组尾(多为花絮/特别篇),不会被选作抽样样本。
func groupByShow(items []scan.Item) []showGroup {
	buckets := map[string][]scan.Item{}
	var keys []string
	for _, it := range items {
		k := showGroupKey(it)
		if _, seen := buckets[k]; !seen {
			keys = append(keys, k)
		}
		buckets[k] = append(buckets[k], it)
	}
	sort.Strings(keys)

	out := make([]showGroup, 0, len(keys))
	for _, k := range keys {
		g := buckets[k]
		sort.SliceStable(g, func(i, j int) bool { return lessEpisode(g[i], g[j]) })
		out = append(out, showGroup{key: k, items: g})
	}
	return out
}

// lessEpisode 是"季→集"排序的比较器,无集号的项排到最后。
func lessEpisode(a, b scan.Item) bool {
	if a.Season != b.Season {
		return a.Season < b.Season
	}
	if a.Episode != b.Episode {
		if a.Episode == 0 {
			return false
		}
		if b.Episode == 0 {
			return true
		}
		return a.Episode < b.Episode
	}
	return a.Path < b.Path
}

// expandInconsistentGroups 找出抽样后仍未形成共识的剧,返回其全部文件的集合。
//
// "未形成共识"= 该剧抽样成功的结果里没有任何两集一致。这种情况下众数不成立,
// 抽样结论不可信,于是整部剧重测(而不是悄悄把某个值套给全剧)。
// 所有剧都有共识时返回 nil。
func expandInconsistentGroups(items []scan.Item, res map[string]auto.Result, sample int) map[string]bool {
	// sample<=1 时无从比较:单个样本的结论就是结论,不再要求多数派。
	if sample <= 1 {
		return nil
	}
	var extra map[string]bool
	for _, g := range groupByShow(items) {
		if hasConsensus(res, g.items) {
			continue
		}
		if extra == nil {
			extra = make(map[string]bool, len(items))
		}
		for _, it := range g.items {
			extra[it.Path] = true
		}
	}
	return extra
}

// hasConsensus 判断这组文件的识别结果里是否存在多数派(至少两集一致)。
func hasConsensus(res map[string]auto.Result, items []scan.Item) bool {
	var oks []auto.Result
	for _, it := range items {
		if r, ok := res[it.Path]; ok && r.OK {
			oks = append(oks, r)
		}
	}
	for i := range oks {
		if agree, _ := auto.Agreement(oks, i); agree >= 2 {
			return true
		}
	}
	return false
}

// applyModeCorrection 对识别结果做众数纠错,并在 Note 上附加一致率。
// ordered 会被就地修改(res 替换为纠错后的结果)。
//
// 关键:纠错必须**按剧分别进行**。同一个目录里可能混排多部剧,各部剧的
// OP/ED 时长本来就不一样,混在一起做众数纠错会把少数剧的切点强行拉向多数剧,
// 反而制造误判。分组维度是 (目录, 剧名, 季)。
func applyModeCorrection(ordered []detectResult) {
	for _, idx := range groupOKResults(ordered) {
		oks := make([]auto.Result, 0, len(idx))
		for _, i := range idx {
			oks = append(oks, ordered[i].Res)
		}
		corrected := auto.Correct(oks) // Correct 不修改入参,返回纠错后的副本
		for ci, oi := range idx {
			r := corrected[ci]
			if len(oks) > 1 {
				agree, total := auto.Agreement(oks, ci) // 一致率按纠错前的原始结果计算
				r.Note = fmt.Sprintf("[一致 %d/%d] ", agree, total) + r.Note
			}
			ordered[oi].Res = r
		}
	}
}

// broadcastShowValues 把抽样集得出的 head/tail 套用到同剧未抽样的集上。
//
// 只覆盖"没抽样所以没有结果"的项(Err==nil 且 Res 未成功);
// 抽样了却识别失败的文件不会被乐观套用 —— 那通常意味着文件本身有问题。
// 剧的结论取该剧成功结果的众数,众数不成立时 none。
func broadcastShowValues(ordered []detectResult) {
	// path → 该剧的样本数量,写进 Note 让用户知道结论的来源。
	for _, idx := range groupOKResults(ordered) {
		if len(idx) == 0 {
			continue
		}
		// 用第一个成功结果作为该剧的代表值(纠错后它们已经一致)。
		rep := ordered[idx[0]].Res
		samples := len(idx)

		key := showGroupKey(ordered[idx[0]].Item)
		for i := range ordered {
			d := &ordered[i]
			if showGroupKey(d.Item) != key {
				continue
			}
			if d.Err != nil || d.Res.OK {
				continue // 已成功或真出错的不动
			}
			d.Res = rep
			d.Res.Fixed = false
			d.Res.Note = fmt.Sprintf("沿用本剧 %d 集抽样结果(未逐集检测)", samples)
		}
	}
}

// agreePrefixRe 匹配 Note 开头的一致率标记,用于推广后重算。
var agreePrefixRe = regexp.MustCompile(`^\[一致 \d+/\d+\]\s*`)

// refreshAgreementNotes 按"全剧"重新计算每条结果的一致率前缀。
//
// applyModeCorrection 只在抽样集里算过一致率(如 [一致 3/3]);
// 推广之后同剧所有集都有结论,分母应该变成真实集数,所以要统一重算一次。
func refreshAgreementNotes(ordered []detectResult) {
	for _, idx := range groupOKResults(ordered) {
		if len(idx) == 0 {
			continue
		}
		oks := make([]auto.Result, 0, len(idx))
		for _, i := range idx {
			oks = append(oks, ordered[i].Res)
		}
		for ci, oi := range idx {
			if len(oks) < 2 {
				break
			}
			agree, total := auto.Agreement(oks, ci)
			note := agreePrefixRe.ReplaceAllString(ordered[oi].Res.Note, "")
			ordered[oi].Res.Note = fmt.Sprintf("[一致 %d/%d] ", agree, total) + note
		}
	}
}

// groupOKResults 返回按剧分组的成功结果下标集合,顺序稳定。
func groupOKResults(ordered []detectResult) [][]int {
	buckets := map[string][]int{}
	var keys []string
	for i, d := range ordered {
		if !d.OK() {
			continue
		}
		k := showGroupKey(d.Item)
		if _, seen := buckets[k]; !seen {
			keys = append(keys, k)
		}
		buckets[k] = append(buckets[k], i)
	}
	sort.Strings(keys)
	out := make([][]int, 0, len(keys))
	for _, k := range keys {
		out = append(out, buckets[k])
	}
	return out
}

// showGroupKey 给出"剧"的唯一标识:目录 + 归一化剧名 + 季号。
// 剧名解析不出来时退化成目录,行为与旧版本一致(同目录算一部剧)。
func showGroupKey(it scan.Item) string {
	return fmt.Sprintf("%s|%s|S%02d",
		filepath.Dir(it.Path), scan.ShowKey(it.Show), it.Season)
}

// fprintln 安全写出一行:输出目标为 nil 时静默跳过。
func fprintln(w io.Writer, s string) {
	if w == nil {
		return
	}
	fmt.Fprintln(w, s)
}

// effectiveWorkers 计算实际并发数:<=0 取 CPU 核数,且不超过任务数。
func effectiveWorkers(workers, n int) int {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if n > 0 && workers > n {
		workers = n
	}
	if workers < 1 {
		workers = 1
	}
	return workers
}

// headTailOverride 把识别成功的结果转成 RunBatch 需要的逐文件头尾覆盖表。
func headTailOverride(results []detectResult) map[string]trim.HeadTail {
	m := make(map[string]trim.HeadTail, len(results))
	for _, d := range results {
		if d.OK() {
			m[d.Item.Path] = trim.HeadTail{
				Head: time.Duration(d.Res.Head * float64(time.Second)),
				Tail: time.Duration(d.Res.Tail * float64(time.Second)),
			}
		}
	}
	return m
}

// failCount 返回识别失败的文件数。
func failCount(results []detectResult) int {
	n := 0
	for _, d := range results {
		if !d.OK() {
			n++
		}
	}
	return n
}

// runnableItems 返回识别成功(可直接裁剪)的文件列表。
func runnableItems(results []detectResult) []scan.Item {
	var items []scan.Item
	for _, d := range results {
		if d.OK() {
			items = append(items, d.Item)
		}
	}
	return items
}
