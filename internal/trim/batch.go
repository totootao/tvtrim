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

// BatchOptions 是批量处理的配置。
type BatchOptions struct {
	Trim      Options
	Workers   int // 并发数,<=0 时按 CPU 核数自动决定
	Verbose   bool
	StopOnErr bool // 遇错是否中止(默认 false,继续处理其余文件)
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
	type probeOut struct {
		item  scan.Item
		probe *ffmpeg.ProbeResult
		err   error
	}
	probeCh := make(chan probeOut, len(items))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for _, it := range items {
		wg.Add(1)
		go func(it scan.Item) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			p, err := runner.Probe(ctx, it.Path)
			probeCh <- probeOut{item: it, probe: p, err: err}
		}(it)
	}
	go func() { wg.Wait(); close(probeCh) }()

	var plans []*Plan
	for po := range probeCh {
		if po.err != nil {
			res.Failed++
			plan := &Plan{Input: po.item.Path, Skip: true, SkipWhy: po.err.Error()}
			plans = append(plans, plan)
			logf("✗ 探测失败 %s: %v", po.item.Path, po.err)
			continue
		}
		if po.item.Size > 0 {
			res.TotalIn += po.item.Size
		}
		p := BuildPlan(po.probe, opts.Trim)
		plans = append(plans, p)
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
	resultCh := make(chan Result, len(runnable))
	var wg2 sync.WaitGroup
	sem2 := make(chan struct{}, workers)

	// 先把探测阶段的失败项放进结果,保证汇总完整。
	res.Results = append(res.Results, failures...)

	for _, p := range runnable {
		wg2.Add(1)
		go func(p *Plan) {
			defer wg2.Done()
			sem2 <- struct{}{}
			defer func() { <-sem2 }()

			t0 := time.Now()
			out, err := exec.Run(ctx, p)
			resultCh <- Result{Plan: p, Output: out, Err: err, Elapsed: time.Since(t0)}
		}(p)
	}
	go func() { wg2.Wait(); close(resultCh) }()

	var mu sync.Mutex
	for r := range resultCh {
		mu.Lock()
		res.Results = append(res.Results, r)
		if r.Err != nil {
			res.Failed++
			logf("✗ 裁剪失败 %s: %v", r.Plan.Input, r.Err)
		} else {
			res.Done++
		}
		mu.Unlock()
	}

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
		mu    sync.Mutex
		plans []*Plan
		wg    sync.WaitGroup
		sem   = make(chan struct{}, workers)
	)
	for _, it := range items {
		wg.Add(1)
		go func(it scan.Item) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			p, err := runner.Probe(ctx, it.Path)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				plans = append(plans, &Plan{Input: it.Path, Skip: true, SkipWhy: err.Error()})
				return
			}
			plans = append(plans, BuildPlan(p, opts))
		}(it)
	}
	wg.Wait()
	return plans
}
