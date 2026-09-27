package main

import (
	"context"
	"fmt"
	"path/filepath"
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

// detectAll 并发执行"探测 + (可选)静音检测 + 多集众数纠错",
// 返回按文件路径排序的结果。mode 为 DetectProbeOnly 时跳过静音检测与识别。
//
// 静音检测需要全片解码音频,是 -auto 模式的主要耗时,因此按 workers 并发处理。
func detectAll(ctx context.Context, runner *ffmpeg.Runner, items []scan.Item, workers int, mode DetectMode) []detectResult {
	workers = effectiveWorkers(workers, len(items))

	outCh := make(chan detectResult, len(items))
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)

	for _, it := range items {
		wg.Add(1)
		go func(it scan.Item) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			probe, err := runner.Probe(ctx, it.Path)
			if err != nil {
				outCh <- detectResult{Item: it, Err: err}
				return
			}
			if mode == DetectProbeOnly {
				outCh <- detectResult{Item: it, Probe: probe}
				return
			}
			total := probe.Duration.Seconds()
			sils, err := runner.DetectSilences(ctx, it.Path, auto.NoiseDB, auto.MinSilence, total)
			if err != nil {
				outCh <- detectResult{Item: it, Probe: probe, Err: err}
				return
			}
			res, err := auto.Detect(total, sils)
			outCh <- detectResult{Item: it, Probe: probe, Res: res, Err: err}
		}(it)
	}
	go func() { wg.Wait(); close(outCh) }()

	var ordered []detectResult
	for d := range outCh {
		ordered = append(ordered, d)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Item.Path < ordered[j].Item.Path })

	if mode == DetectSilence {
		applyModeCorrection(ordered)
	}
	return ordered
}

// applyModeCorrection 对识别结果做众数纠错,并在 Note 上附加一致率。
// ordered 会被就地修改(res 替换为纠错后的结果)。
//
// 关键:纠错必须**按剧分别进行**。同一个目录里可能混排多部剧,各部剧的
// OP/ED 时长本来就不一样,混在一起做众数纠错会把少数剧的切点强行拉向多数剧,
// 反而制造误判。分组维度是 (目录, 剧名, 季)。
func applyModeCorrection(ordered []detectResult) {
	groups := map[string][]int{}
	var keys []string
	for i, d := range ordered {
		if !d.OK() {
			continue
		}
		k := showGroupKey(d.Item)
		if _, seen := groups[k]; !seen {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], i)
	}
	sort.Strings(keys)

	for _, k := range keys {
		okIdx := groups[k]
		oks := make([]auto.Result, 0, len(okIdx))
		for _, i := range okIdx {
			oks = append(oks, ordered[i].Res)
		}
		corrected := auto.Correct(oks) // Correct 不修改入参,返回纠错后的副本
		for ci, oi := range okIdx {
			r := corrected[ci]
			if len(oks) > 1 {
				agree, total := auto.Agreement(oks, ci) // 一致率按纠错前的原始结果计算
				r.Note = fmt.Sprintf("[一致 %d/%d] ", agree, total) + r.Note
			}
			ordered[oi].Res = r
		}
	}
}

// showGroupKey 给出"剧"的唯一标识:目录 + 归一化剧名 + 季号。
// 剧名解析不出来时退化成目录,行为与旧版本一致(同目录算一部剧)。
func showGroupKey(it scan.Item) string {
	return fmt.Sprintf("%s|%s|S%02d",
		filepath.Dir(it.Path), scan.ShowKey(it.Show), it.Season)
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
