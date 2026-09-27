package trim

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/totootao/tvtrim/internal/ffmpeg"
	"github.com/totootao/tvtrim/internal/scan"
)

// statSize 返回文件字节数,失败返回 0。
func statSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// HeadTail 是一对头尾时长,用于逐文件覆盖全局设置(自动识别模式)。
type HeadTail struct {
	Head time.Duration
	Tail time.Duration
}

// BatchOptions 是批量处理的配置。
type BatchOptions struct {
	Trim      Options
	Workers   int // 并发数,<=0 时按 CPU 核数自动决定
	Verbose   bool
	StopOnErr bool // 遇错是否中止(默认 false,继续处理其余文件)
	// HeadTailOverride 为个别文件指定头尾时长,key 为文件路径
	// (自动识别模式下每个文件的切点不同)。未列出的文件用 Trim.Head/Trim.Tail。
	HeadTailOverride map[string]HeadTail
	// OnResult 在每个文件处理完成后被调用,用于外部进度展示(Web 界面用)。
	// 注意:该函数在 RunBatch 的结果收集协程中同步调用,实现应避免耗时操作。
	OnResult func(Result)
}

// BatchResult 汇总一次批量处理的结果。
type BatchResult struct {
	Results  []Result
	Total    int
	Done     int
	Skipped  int
	Failed   int
	Elapsed  time.Duration
	TotalIn  int64 // 输入总字节数
	TotalOut int64 // 输出总字节数
}

// indexedResult 记住结果对应的序号,好让最终 res.Results 的顺序
// 跟着输入走 —— 否则每次运行的汇总表顺序都不一样。
type indexedResult struct {
	i   int
	res Result
}

// RunBatch 对文件列表并发执行裁剪。
//
// 流程:先并行探测(读时长),据此计算计划并过滤掉需要跳过的文件,
// 再并行执行裁剪。ffmpeg 的 stream copy 是 IO 密集型的,
// 并发数默认取 CPU 核数,可通过 Workers 调整。
func RunBatch(ctx context.Context, runner *ffmpeg.Runner, items []scan.Item, opts BatchOptions) *BatchResult {
	started := time.Now()
	res := &BatchResult{Total: len(items)}

	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers > len(items) && len(items) > 0 {
		workers = len(items)
	}
	if workers < 1 {
		workers = 1
	}

	logf := func(format string, args ...any) {
		if opts.Verbose {
			fmt.Printf(format+"\n", args...)
		}
	}

	// 阶段一:并行探测时长。
	// 按输入顺序预占位:并发完成有先后,但 plans 的顺序必须与 items 一致,
	// 否则同一个命令跑两次,计划表里的行会换来换去,输出没法 diff。
	type probeOut struct {
		idx   int
		item  scan.Item
		probe *ffmpeg.ProbeResult
		err   error
	}
	probeCh := make(chan probeOut, len(items))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for i, it := range items {
		wg.Add(1)
		go func(i int, it scan.Item) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			p, err := runner.Probe(ctx, it.Path)
			probeCh <- probeOut{idx: i, item: it, probe: p, err: err}
		}(i, it)
	}
	go func() { wg.Wait(); close(probeCh) }()

	plans := make([]*Plan, len(items))
	for po := range probeCh {
		if po.err != nil {
			res.Failed++
			plans[po.idx] = &Plan{Input: po.item.Path, Skip: true, SkipWhy: po.err.Error()}
			logf("✗ 探测失败 %s: %v", po.item.Path, po.err)
			continue
		}
		if po.item.Size > 0 {
			res.TotalIn += po.item.Size
		}
		t := opts.Trim
		if ht, ok := opts.HeadTailOverride[po.item.Path]; ok {
			t.Head, t.Tail = ht.Head, ht.Tail
		}
		plans[po.idx] = BuildPlan(po.probe, t)
	}

	// 阶段二:区分可执行项与被跳过项。
	// 注意:探测失败的项也要进 Results,否则汇总里看不到失败原因。
	failures := make([]Result, 0)
	var runnable []*Plan
	for _, p := range plans {
		if p.Skip {
			if p.Probe != nil {
				res.Skipped++
			}
			// Probe 为 nil 表示探测阶段就失败了(plan.Input 是文件路径)。
			if p.Probe == nil {
				failures = append(failures, Result{
					Plan: p,
					Err:  fmt.Errorf("%s", p.SkipWhy),
				})
			}
			continue
		}
		runnable = append(runnable, p)
	}

	if opts.Trim.DryRun {
		res.Results = make([]Result, 0, len(plans))
		for _, p := range plans {
			res.Results = append(res.Results, Result{Plan: p})
		}
		res.Elapsed = time.Since(started)
		return res
	}

	// 阶段三:并行裁剪。
	exec := &Executor{FFmpeg: runner, Opts: opts.Trim, Log: nil}
	resultOut := make([]Result, len(runnable))
	resultCh := make(chan indexedResult, len(runnable))
	var wg2 sync.WaitGroup
	sem2 := make(chan struct{}, workers)

	// 先把探测阶段的失败项放进结果,保证汇总完整。
	res.Results = append(res.Results, failures...)

	for i, p := range runnable {
		wg2.Add(1)
		go func(i int, p *Plan) {
			defer wg2.Done()
			sem2 <- struct{}{}
			defer func() { <-sem2 }()

			t0 := time.Now()
			out, err := exec.Run(ctx, p)
			resultCh <- indexedResult{i: i, res: Result{Plan: p, Output: out, Err: err, Elapsed: time.Since(t0)}}
		}(i, p)
	}
	go func() { wg2.Wait(); close(resultCh) }()

	var mu sync.Mutex
	for ir := range resultCh {
		r := ir.res
		resultOut[ir.i] = r
		mu.Lock()
		if r.Err != nil {
			res.Failed++
			logf("✗ 裁剪失败 %s: %v", r.Plan.Input, r.Err)
		} else {
			res.Done++
		}
		cb := opts.OnResult
		mu.Unlock()
		if cb != nil {
			cb(r)
		}
	}
	res.Results = append(res.Results, resultOut...)

	// 汇总输出体积。
	mu.Lock()
	for _, r := range res.Results {
		if r.Err == nil && r.Output != "" {
			if st, err := statSize(r.Output); err == nil {
				res.TotalOut += st
			}
		}
	}
	mu.Unlock()

	res.Elapsed = time.Since(started)
	return res
}

// Plans 为文件列表构建计划(仅探测,不执行),供 dry-run 与外部预览使用。
func Plans(ctx context.Context, runner *ffmpeg.Runner, items []scan.Item, opts Options, workers int) []*Plan {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, workers)
	)
	plans := make([]*Plan, len(items))
	for i, it := range items {
		wg.Add(1)
		go func(i int, it scan.Item) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			p, err := runner.Probe(ctx, it.Path)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				plans[i] = &Plan{Input: it.Path, Skip: true, SkipWhy: err.Error()}
				return
			}
			plans[i] = BuildPlan(p, opts)
		}(i, it)
	}
	wg.Wait()
	return plans
}
