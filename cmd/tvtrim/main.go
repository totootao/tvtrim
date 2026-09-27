// Command tvtrim 批量去掉电视剧剧集的片头与片尾。
//
// 用法示例:
//
//	tvtrim --head 90 --tail 60 ./某剧第一季/
//	tvtrim --head 1m30s --tail 45s -r ./剧集目录/ --dry-run
//	tvtrim --head 90 --tail 60 --inplace episode01.mkv
//
// 实现上只做 stream copy(不重编码),因此速度接近磁盘拷贝。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/totootao/tvtrim/internal/auto"
	"github.com/totootao/tvtrim/internal/ffmpeg"
	"github.com/totootao/tvtrim/internal/scan"
	"github.com/totootao/tvtrim/internal/trim"
	"github.com/totootao/tvtrim/internal/web"
)

// version 在构建时可通过 -ldflags 注入。
var version = "1.4.2"

const usage = `tvtrim - 电视剧剧集去头去尾(基于 ffmpeg-trim,零重编码)

用法:
  tvtrim [选项] <文件或目录> [更多文件或目录...]

选项:
  -auto            自动识别片头/片尾(基于静音检测,多集自动纠错)
  -web             启动 Web 界面:扫描后人工按剧确认再执行
  -addr <地址>     Web 监听地址,默认宿机 127.0.0.1:0(端口自动分配)、
                   容器内 0.0.0.0:8080
  -no-open         -web 时不自动打开浏览器(容器内自动不尝试)
  -head <时长>     要砍掉的片头时长,如 90、1m30s、0:90
  -tail <时长>     要砍掉的片尾时长,如 60、1m、0:45
  -keep <时长>     结尾额外保留的安全余量(会让输出更短,默认 0)
  -suffix <后缀>   输出文件名后缀,默认 "-trim"
  -inplace         原地替换原文件(先写临时文件再原子替换)
  -o <路径>        显式指定输出文件(仅单文件模式)
  -r               递归处理子目录
  -j <数量>        并发数,默认 CPU 核数
  -dry-run         只显示将要执行的操作,不实际裁剪
  -overwrite       允许覆盖已存在的输出文件
  -min <时长>      输出时长下限,低于此值则跳过,默认 10s
  -ffmpeg <路径>   指定 ffmpeg 可执行文件
  -no-download     禁止自动下载 ffmpeg-trim
  -v               输出详细信息
  -version         显示版本
  -h, --help       显示帮助

时长格式:
  90        90 秒
  1m30s     1 分 30 秒
  1:30      1 分 30 秒
  0:01:30   1 分 30 秒(时:分:秒)

示例:
  # 砍掉 90 秒片头和 60 秒片尾,结果存为 xxx-trim.mp4
  tvtrim -head 90 -tail 60 ./进击的巨人.S01/

  # 自动识别每集的片头曲/片尾曲并裁剪(推荐先 dry-run 看识别结果)
  tvtrim -auto ./某剧.S01/ -dry-run

  # 打开 Web 界面:自动识别后在页面上逐剧确认,点"确认执行本剧"才真正裁剪
  tvtrim -web -auto ./某剧全集/ -r

  # Web 界面 + 手动时长(页面上仍可逐集调整)
  tvtrim -web -head 90 -tail 60 ./某剧.S01/

  # 递归处理整个剧,先看一眼会做什么
  tvtrim -r -head 1m30s -tail 45s ./某剧/ -dry-run

  # 直接原地替换(注意备份!)
  tvtrim -head 90 -tail 60 -inplace -overwrite ./S01E01.mkv

环境变量:
  TVTRIM_FFMPEG   指定 ffmpeg 路径
  TVTRIM_PROXY    GitHub 代理前缀,默认 %s
                  设为 "none" 表示直连
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "\n错误: %v\n", err)
		os.Exit(1)
	}
}

// cliOptions 保存解析后的命令行参数。
type cliOptions struct {
	auto       bool
	head       string
	tail       string
	keep       string
	suffix     string
	inplace    bool
	output     string
	recursive  bool
	workers    int
	dryRun     bool
	overwrite  bool
	minDur     string
	ffmpegPath string
	noDownload bool
	verbose    bool
	showVer    bool
	web        bool
	addr       string
	noOpen     bool
}

func run(argv []string) error {
	var o cliOptions

	fs := flag.NewFlagSet("tvtrim", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, usage, ffmpeg.DefaultProxy) }

	fs.BoolVar(&o.auto, "auto", false, "自动识别片头/片尾(静音检测+多集纠错)")
	fs.BoolVar(&o.web, "web", false, "启动 Web 界面:扫描后人工确认每部剧再执行")
	fs.StringVar(&o.addr, "addr", "", "Web 监听地址(默认宿机 127.0.0.1:0,容器内 0.0.0.0:8080)")
	fs.StringVar(&o.head, "head", "", "要砍掉的片头时长(如 90 / 1m30s)")
	fs.StringVar(&o.tail, "tail", "", "要砍掉的片尾时长(如 60 / 1m)")
	fs.StringVar(&o.keep, "keep", "", "结尾额外保留的安全余量")
	fs.StringVar(&o.suffix, "suffix", trim.DefaultSuffix, "输出文件名后缀")
	fs.BoolVar(&o.inplace, "inplace", false, "原地替换原文件")
	fs.StringVar(&o.output, "o", "", "显式指定输出文件(仅单文件模式)")
	fs.BoolVar(&o.recursive, "r", false, "递归处理子目录")
	fs.IntVar(&o.workers, "j", 0, "并发数,默认 CPU 核数")
	fs.BoolVar(&o.dryRun, "dry-run", false, "只预览不执行")
	fs.BoolVar(&o.overwrite, "overwrite", false, "允许覆盖已存在的输出")
	fs.StringVar(&o.minDur, "min", "10s", "输出时长下限")
	fs.StringVar(&o.ffmpegPath, "ffmpeg", "", "指定 ffmpeg 可执行文件")
	fs.BoolVar(&o.noDownload, "no-download", false, "禁止自动下载 ffmpeg-trim")
	fs.BoolVar(&o.verbose, "v", false, "输出详细信息")
	fs.BoolVar(&o.noOpen, "no-open", false, "-web 时不尝试自动打开浏览器")
	fs.BoolVar(&o.showVer, "version", false, "显示版本")

	if err := fs.Parse(argv); err != nil {
		return nil // flag 已打印错误与用法
	}

	if o.showVer {
		fmt.Printf("tvtrim %s (ffmpeg-trim 零重编码裁剪)\n", version)
		return nil
	}

	inputs := fs.Args()
	if len(inputs) == 0 {
		fmt.Fprintf(os.Stderr, usage, ffmpeg.DefaultProxy)
		return fmt.Errorf("请至少指定一个文件或目录")
	}

	// 解析时长参数。
	head, err := parseDuration(o.head)
	if err != nil {
		return fmt.Errorf("-head 参数无效: %w", err)
	}
	tail, err := parseDuration(o.tail)
	if err != nil {
		return fmt.Errorf("-tail 参数无效: %w", err)
	}
	keep, err := parseDuration(o.keep)
	if err != nil {
		return fmt.Errorf("-keep 参数无效: %w", err)
	}
	minDur, err := parseDuration(o.minDur)
	if err != nil {
		return fmt.Errorf("-min 参数无效: %w", err)
	}
	// -web 可以与 -auto(页面展示识别结果)或 -head/-tail(页面可微调)组合,
	// 但二者不能同时给;单独 -web 也无意义。
	if o.web && o.auto && (head != 0 || tail != 0) {
		return fmt.Errorf("-web 模式下请二选一:-auto 自动识别,或用 -head/-tail 手动指定")
	}
	if o.auto && (head != 0 || tail != 0) && !o.web {
		return fmt.Errorf("-auto 与 -head/-tail 不能同时使用(自动识别模式下切点由检测决定)")
	}
	if head == 0 && tail == 0 && !o.auto && !o.web {
		return fmt.Errorf("请至少指定 -head 或 -tail 中的一个,或使用 -auto / -web")
	}
	if o.web && head == 0 && tail == 0 && !o.auto {
		return fmt.Errorf("-web 需要配合 -auto 或 -head/-tail:页面上至少要有一个默认切点")
	}
	if head < 0 || tail < 0 || keep < 0 {
		return fmt.Errorf("时长不能为负数")
	}
	if o.output != "" && len(inputs) > 1 {
		return fmt.Errorf("-o 只能在处理单个文件时使用")
	}
	if o.output != "" && o.inplace {
		return fmt.Errorf("-o 与 -inplace 不能同时使用")
	}
	if o.inplace && !o.overwrite {
		fmt.Fprintln(os.Stderr, "提示: -inplace 会覆盖原文件,已自动开启 -overwrite")
		o.overwrite = true
	}

	// 处理中断信号,保证能优雅退出。
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 定位 ffmpeg(可能在探测后才知道要不要下载,这里先准备好)。
	runner, err := ffmpeg.Locate(ctx, ffmpeg.LocateOptions{
		Explicit:   o.ffmpegPath,
		NoDownload: o.noDownload,
		Progress:   func(msg string) { fmt.Println(msg) },
	})
	if err != nil {
		return err
	}
	runner.Verbose = o.verbose

	// 扫描待处理文件。
	suffixForExclude := o.suffix
	if suffixForExclude == "" || suffixForExclude == "inplace" {
		suffixForExclude = ""
	}
	items, err := scan.Collect(inputs, scan.Options{
		Recursive:     o.recursive,
		ExcludeSuffix: suffixForExclude,
	})
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("没有找到可处理的媒体文件(支持: %s)",
			strings.Join(ffmpeg.SupportedExts, " "))
	}

	trimOpts := trim.Options{
		Head:        head,
		Tail:        tail,
		Keep:        keep,
		MinDuration: minDur,
		Overwrite:   o.overwrite,
		DryRun:      o.dryRun,
		Verbose:     o.verbose,
		Suffix:      o.suffix,
		Output:      o.output,
	}
	if o.inplace {
		trimOpts.Suffix = "inplace"
	}

	fmt.Printf("tvtrim %s\n", version)
	if o.auto {
		fmt.Printf("  模式    : 自动识别片头/片尾(静音检测)\n")
	} else {
		fmt.Printf("  片头砍掉: %s\n", fmtDur(head))
		fmt.Printf("  片尾砍掉: %s\n", fmtDur(tail))
	}
	if keep > 0 {
		fmt.Printf("  结尾余量: %s\n", fmtDur(keep))
	}
	fmt.Printf("  ffmpeg  : %s\n", runner.Path)
	// 一个目录里可能混排多部剧,明确告诉用户识别出了几部,便于核对分组是否正确。
	fmt.Printf("  待处理  : %d 个文件 / %d 部剧\n", len(items), scan.CountShows(items))
	fmt.Printf("  输出方式: %s\n", describeOutput(o))
	if o.dryRun {
		fmt.Printf("  模式    : 预览(dry-run)\n")
	}
	fmt.Println()

	// Web 界面优先于 dry-run/自动识别分支:先出给人看,再等人工确认。
	if o.web {
		return runWeb(ctx, runner, items, o, trimOpts, head, tail)
	}

	// 自动识别优先于普通 dry-run:runAuto 内部自己处理 dry-run,
	// 既要打印识别结果表,也要打印裁剪计划(否则 -auto -dry-run 会因为
	// 还没算切点而把所有文件标成"未指定头尾时长")。
	if o.auto {
		return runAuto(ctx, runner, items, o, trimOpts)
	}

	// dry-run 走单独路径:探测 + 展示计划,不执行。
	if o.dryRun {
		return runDryRun(ctx, runner, items, trimOpts)
	}

	batch := trim.RunBatch(ctx, runner, items, trim.BatchOptions{
		Trim:    trimOpts,
		Workers: o.workers,
		Verbose: o.verbose,
	})

	printBatchSummary(batch, o.suffix, o.inplace)

	if ctx.Err() != nil {
		return fmt.Errorf("已中断")
	}
	if batch.Failed > 0 {
		return fmt.Errorf("%d 个文件处理失败", batch.Failed)
	}
	return nil
}

// runAuto 执行自动识别模式:
//
//	阶段一 并发探测 + 静音检测 + 多集众数纠错(detectAll)
//	阶段二 展示识别结果
//	阶段三 按每集切点进入常规批量裁剪
func runAuto(ctx context.Context, runner *ffmpeg.Runner, items []scan.Item, o cliOptions, trimOpts trim.Options) error {
	results := detectAll(ctx, runner, items, o.workers, DetectSilence)

	// 阶段二:展示识别结果。
	runItems := runnableItems(results)
	override := headTailOverride(results)
	if len(runItems) == 0 {
		return fmt.Errorf("自动识别全部失败,请改用 -head/-tail 手动指定")
	}

	fmt.Printf("自动识别结果(静音阈值 %gdB, 切点静音 ≥%.1fs):\n\n", auto.NoiseDB, auto.MinSilence)
	fmt.Printf("  %-20s %-22s %9s %9s  %s\n", "剧集", "文件", "-head", "-tail", "说明")
	fmt.Println("  " + strings.Repeat("-", 100))
	failN := failCount(results)
	for _, d := range results {
		// 同目录混排多部剧时,剧名列是核对分组是否正确的关键。
		show := shortName(showNameOf(d.Item), 18)
		name := shortName(filepath.Base(d.Item.Path), 20)
		switch {
		case d.Err != nil:
			fmt.Printf("  %-20s %-22s %10s %10s  ✗ %v\n", show, name, "-", "-", d.Err)
		case !d.Res.OK:
			fmt.Printf("  %-20s %-22s %10s %10s  ✗ %s\n", show, name, "-", "-", d.Res.Note)
		default:
			note := d.Res.Note
			if note == "" {
				note = "OK"
			}
			fmt.Printf("  %-20s %-22s %9.1fs %9.1fs  %s\n", show, name, d.Res.Head, d.Res.Tail, note)
		}
	}
	fmt.Println("  " + strings.Repeat("-", 100))
	if failN > 0 {
		fmt.Printf("\n提示: %d 个文件识别失败,已跳过;其余 %d 个将按识别结果裁剪。\n", failN, len(runItems))
	}

	// 阶段三:按每集切点进入常规批量流程。
	batch := trim.RunBatch(ctx, runner, runItems, trim.BatchOptions{
		Trim:             trimOpts,
		Workers:          o.workers,
		Verbose:          o.verbose,
		HeadTailOverride: override,
	})

	if o.dryRun {
		var plans []*trim.Plan
		for _, r := range batch.Results {
			plans = append(plans, r.Plan)
		}
		fmt.Println()
		printPlans(plans)
		fmt.Println("\n这是 dry-run,未做任何修改。去掉 -dry-run 即可实际执行。")
		return nil
	}

	printBatchSummary(batch, o.suffix, o.inplace)
	if ctx.Err() != nil {
		return fmt.Errorf("已中断")
	}
	if batch.Failed > 0 {
		return fmt.Errorf("%d 个文件处理失败", batch.Failed)
	}
	return nil
}

// showNameOf 给出用于表格展示的剧名,解析不出时退回目录名。
func showNameOf(it scan.Item) string {
	if it.Show != "" {
		return it.Show
	}
	return filepath.Base(filepath.Dir(it.Path))
}

// runWeb 启动本地 Web 界面:
// 扫描/识别完成后把结果推给页面,由人工按剧勾选确认后再真正执行裁剪。
// 阻塞直到用户 Ctrl+C 或在页面上点击退出。
func runWeb(ctx context.Context, runner *ffmpeg.Runner, items []scan.Item,
	o cliOptions, trimOpts trim.Options, head, tail time.Duration) error {

	mode, detectMode := web.ModeManual, DetectProbeOnly
	if o.auto {
		mode, detectMode = web.ModeAuto, DetectSilence
	}

	fmt.Printf("正在%s(%d 个文件)…\n", map[bool]string{true: "自动识别切点", false: "探测媒体信息"}[o.auto], len(items))
	results := detectAll(ctx, runner, items, o.workers, detectMode)

	probes := make(map[string]*ffmpeg.ProbeResult, len(results))
	detects := make(map[string]auto.Result, len(results))
	for _, d := range results {
		if d.Probe != nil {
			probes[d.Item.Path] = d.Probe
		}
		if detectMode == DetectSilence && d.OK() {
			detects[d.Item.Path] = d.Res
		}
	}
	if detectMode == DetectSilence {
		ok := len(detects)
		fail := len(results) - ok
		fmt.Printf("识别完成: %d 成功", ok)
		if fail > 0 {
			fmt.Printf(", %d 失败(页面上不可勾选)", fail)
		}
		fmt.Println()
	}

	shows := web.BuildShows(items, probes, detects, head.Seconds(), tail.Seconds())
	srv := web.New(runner, shows, trimOpts, o.workers, version, mode)

	// -addr 留空时按运行环境决定:容器里监听 0.0.0.0:8080,否则监听回环随机端口。
	addr := resolveWebAddr(o.addr)
	ln, err := srv.Listen(addr)
	if err != nil {
		return fmt.Errorf("启动 Web 服务失败: %w", err)
	}
	actual := ln.Addr().String()
	url := fmt.Sprintf("http://%s/", actual)
	fmt.Printf("  Web 界面: %s\n", url)
	// 监听地址是通配地址(容器里的默认值)时,提示容器外怎么访问。
	if hint := externalURLHint(actual); hint != "" {
		fmt.Printf("  容器模式: %s\n", hint)
	}
	fmt.Printf("  输出方式: %s\n", describeOutput(o))
	fmt.Println("\n在浏览器里勾选要处理的剧集,确认后执行。Ctrl+C 或点页面上的\"退出\"结束。")

	if !o.noOpen && !inContainer() {
		go tryOpenBrowser(url)
	}
	return srv.Serve(ctx, ln)
}

// tryOpenBrowser 尽力打开系统浏览器,失败无所谓(用户可手动访问)。
func tryOpenBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return // 静默失败
	}
	_ = cmd.Process.Release()
}

// printPlans 打印裁剪计划表格,返回 (将执行, 跳过) 数量。
func printPlans(plans []*trim.Plan) (willRun, willSkip int) {
	fmt.Printf("%-16s %-30s %12s %10s %10s %10s\n", "剧集", "文件", "总时长", "起点", "终点", "裁剪后")
	fmt.Println(strings.Repeat("-", 94))

	for _, p := range plans {
		// 剧名从文件名解析(scan.ShowOf),解析不出时用目录名兜底。
		show := shortName(showNameOf(scan.Item{Path: p.Input, Show: scan.ShowOf(filepath.Base(p.Input))}), 14)
		name := shortName(p.Input, 28)
		if p.Skip {
			willSkip++
			fmt.Printf("%-16s %-30s %12s %s\n", show, name, "-", "跳过: "+p.SkipWhy)
			continue
		}
		willRun++
		fmt.Printf("%-16s %-30s %12s %10s %10s %10s\n",
			show,
			name,
			fmtDur(p.Duration),
			fmtDur(p.Start),
			fmtDur(p.End),
			fmtDur(p.OutLength),
		)
	}
	fmt.Println(strings.Repeat("-", 94))
	fmt.Printf("共 %d 个文件: %d 个将被裁剪, %d 个跳过\n", len(plans), willRun, willSkip)
	return willRun, willSkip
}

// runDryRun 只展示每个文件的裁剪计划。
func runDryRun(ctx context.Context, runner *ffmpeg.Runner, items []scan.Item, opts trim.Options) error {
	plans := trim.Plans(ctx, runner, items, opts, 0)
	printPlans(plans)
	fmt.Println("\n这是 dry-run,未做任何修改。去掉 -dry-run 即可实际执行。")
	return nil
}

// printBatchSummary 打印批量处理结果汇总。
func printBatchSummary(b *trim.BatchResult, suffix string, inplace bool) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("完成: %d 成功", b.Done)
	if b.Skipped > 0 {
		fmt.Printf(", %d 跳过", b.Skipped)
	}
	if b.Failed > 0 {
		fmt.Printf(", %d 失败", b.Failed)
	}
	fmt.Printf(", 耗时 %s\n", b.Elapsed.Round(time.Millisecond))

	if b.TotalIn > 0 && b.TotalOut > 0 {
		fmt.Printf("体积: %s -> %s\n", humanSize(b.TotalIn), humanSize(b.TotalOut))
	}

	// 打印失败清单,方便定位。
	if b.Failed > 0 {
		fmt.Println("\n失败明细:")
		for _, r := range b.Results {
			if r.Err != nil {
				fmt.Printf("  ✗ %s\n    %v\n", r.Plan.Input, r.Err)
			}
		}
	}

	// 打印跳过清单。
	var skipped []trim.Result
	for _, r := range b.Results {
		if r.Plan != nil && r.Plan.Skip && r.Err == nil {
			skipped = append(skipped, r)
		}
	}
	if len(skipped) > 0 {
		fmt.Println("\n跳过明细:")
		for _, r := range skipped {
			fmt.Printf("  - %s: %s\n", shortName(r.Plan.Input, 50), r.Plan.SkipWhy)
		}
	}

	if !inplace && b.Done > 0 {
		fmt.Printf("\n输出文件已按 \"%s\" 后缀生成,确认无误后可自行删除原文件。\n", suffix)
	}
}

// parseDuration 解析人类可读的时长。
//
// 支持:
//
//	"90"        -> 90s
//	"90s"       -> 90s
//	"1m30s"     -> 90s
//	"1h2m3s"    -> 3723s
//	"1:30"      -> 90s      (分:秒)
//	"0:01:30"   -> 90s      (时:分:秒)
//	"1.5s"      -> 1.5s
//	"0.5m"      -> 30s
//	""          -> 0
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	// 纯数字:按秒处理。
	if isAllDigits(s) {
		n, err := parseFloat(s)
		if err != nil {
			return 0, err
		}
		return time.Duration(n * float64(time.Second)), nil
	}

	// 冒号分隔:MM:SS 或 HH:MM:SS。
	if strings.Contains(s, ":") {
		return parseClock(s)
	}

	// 带单位后缀:1h2m3s。
	return parseUnits(s)
}

// parseClock 解析 1:30 或 0:01:30 形式。
func parseClock(s string) (time.Duration, error) {
	parts := strings.Split(s, ":")
	if len(parts) > 3 || len(parts) < 2 {
		return 0, fmt.Errorf("无法识别的时长: %q", s)
	}
	vals := make([]float64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return 0, fmt.Errorf("无法识别的时长: %q", s)
		}
		v, err := parseFloat(p)
		if err != nil {
			return 0, fmt.Errorf("无法识别的时长: %q", s)
		}
		if v < 0 {
			return 0, fmt.Errorf("时长不能为负: %q", s)
		}
		vals = append(vals, v)
	}

	var total float64
	if len(vals) == 2 {
		total = vals[0]*60 + vals[1]
	} else {
		total = vals[0]*3600 + vals[1]*60 + vals[2]
	}
	return time.Duration(total * float64(time.Second)), nil
}

// parseUnits 解析 1h2m3s / 90s / 1.5m 形式。
func parseUnits(s string) (time.Duration, error) {
	var total time.Duration
	i := 0
	matched := false

	for i < len(s) {
		// 跳过空格
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}

		// 读数字
		j := i
		for j < len(s) && (isDigit(s[j]) || s[j] == '.') {
			j++
		}
		if j == i {
			return 0, fmt.Errorf("无法识别的时长: %q", s)
		}
		v, err := parseFloat(s[i:j])
		if err != nil {
			return 0, fmt.Errorf("无法识别的时长: %q", s)
		}

		// 读单位
		k := j
		for k < len(s) && !isDigit(s[k]) && s[k] != '.' {
			k++
		}
		unit := strings.ToLower(strings.TrimSpace(s[j:k]))
		if unit == "" {
			unit = "s"
		}

		var mul time.Duration
		switch unit {
		case "h", "hr", "hour", "小时", "时":
			mul = time.Hour
		case "m", "min", "分钟", "分":
			mul = time.Minute
		case "s", "sec", "秒":
			mul = time.Second
		case "ms", "毫秒":
			mul = time.Millisecond
		default:
			return 0, fmt.Errorf("未知的时间单位 %q(可用 h/m/s)", unit)
		}
		total += time.Duration(v * float64(mul))
		matched = true
		i = k
	}

	if !matched {
		return 0, fmt.Errorf("无法识别的时长: %q", s)
	}
	return total, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) && s[i] != '.' {
			return false
		}
	}
	return len(s) > 0
}

func parseFloat(s string) (float64, error) {
	var (
		intPart, fracPart float64
		seenDot           bool
		div               = 1.0
	)
	if s == "" {
		return 0, fmt.Errorf("空数字")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '.' {
			if seenDot {
				return 0, fmt.Errorf("非法数字 %q", s)
			}
			seenDot = true
			continue
		}
		if !isDigit(c) {
			return 0, fmt.Errorf("非法数字 %q", s)
		}
		if seenDot {
			div *= 10
			fracPart += float64(c-'0') / div
		} else {
			intPart = intPart*10 + float64(c-'0')
		}
	}
	return intPart + fracPart, nil
}

// fmtDur 把时长格式化成 1h02m03s / 45s 这种易读形式。
func fmtDur(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	if d < 0 {
		return "-" + fmtDur(-d)
	}
	d = d.Round(time.Millisecond)

	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := float64(d) / float64(time.Second)

	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02.0fs", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%04.1fs", m, s)
	default:
		return fmt.Sprintf("%.2fs", s)
	}
}

// describeOutput 描述本次的输出方式。
func describeOutput(o cliOptions) string {
	switch {
	case o.inplace:
		return "原地替换原文件(先写临时文件)"
	case o.output != "":
		return fmt.Sprintf("输出到 %s", o.output)
	default:
		return fmt.Sprintf("同目录生成,后缀 %q", o.suffix)
	}
}

// shortName 截断过长文件名,保留尾部(尾部信息量更大)。
func shortName(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return "…" + string(r[len(r)-max+1:])
}

// humanSize 把字节数格式化成人类可读大小。
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 3 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGT"[exp])
}
