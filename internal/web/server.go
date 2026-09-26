package web

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/totootao/tvtrim/internal/ffmpeg"
	"github.com/totootao/tvtrim/internal/scan"
	"github.com/totootao/tvtrim/internal/trim"
)

//go:embed ui.html
var uiFS embed.FS

// Mode 描述切点来源,页面据此提示用户。
type Mode string

const (
	ModeAuto   Mode = "auto"   // 静音检测自动识别
	ModeManual Mode = "manual" // 用户手动指定统一时长
)

// ItemStatus 是单个文件的执行状态,供前端渲染。
type ItemStatus struct {
	Path   string  `json:"path"`
	Name   string  `json:"name"`
	Status string  `json:"status"` // pending | running | done | failed
	Msg    string  `json:"msg"`
	Output string  `json:"output"`
	Secs   float64 `json:"secs"`
}

// JobState 是一次执行任务的进度快照。
type JobState struct {
	Running  bool         `json:"running"`
	Finished bool         `json:"finished"`
	Canceled bool         `json:"canceled"`
	Total    int          `json:"total"`
	Done     int          `json:"done"`
	Failed   int          `json:"failed"`
	Started  time.Time    `json:"started,omitempty"`
	Elapsed  float64      `json:"elapsed"`
	BytesIn  int64        `json:"bytes_in"`
	BytesOut int64        `json:"bytes_out"`
	Items    []ItemStatus `json:"items,omitempty"`
	Error    string       `json:"error,omitempty"`
}

// Server 提供扫描结果预览与人工确认执行的 HTTP 服务。
type Server struct {
	ff      *ffmpeg.Runner
	shows   []Show
	opts    trim.Options
	workers int
	version string
	mode    Mode
	summary Summary

	mu     sync.Mutex
	job    *JobState
	cancel context.CancelFunc
	// onShutdown 由 Serve 填充,页面可通过 /api/shutdown 触发退出。
	onShutdown func()
}

// New 构造 web 服务。opts 中的 DryRun 会被强制为 false(Web 界面执行真实操作),
// Overwrite 沿用命令行设置。
func New(ff *ffmpeg.Runner, shows []Show, opts trim.Options, workers int, version string, mode Mode) *Server {
	opts.DryRun = false
	if workers <= 0 {
		workers = 4
	}
	return &Server{
		ff:      ff,
		shows:   shows,
		opts:    opts,
		workers: workers,
		version: version,
		mode:    mode,
		summary: Summarize(shows),
	}
}

// Handler 返回 HTTP 路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/run", s.handleRun)
	mux.HandleFunc("/api/progress", s.handleProgress)
	mux.HandleFunc("/api/cancel", s.handleCancel)
	mux.HandleFunc("/api/shutdown", s.handleShutdown)
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, _ := uiFS.ReadFile("ui.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// stateResponse 是 /api/state 的响应。
type stateResponse struct {
	Version string    `json:"version"`
	Mode    Mode      `json:"mode"`
	FFmpeg  string    `json:"ffmpeg"`
	Summary Summary   `json:"summary"`
	Shows   []Show    `json:"shows"`
	Job     *JobState `json:"job"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "仅支持 GET", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, stateResponse{
		Version: s.version,
		Mode:    s.mode,
		FFmpeg:  s.ff.Path,
		Summary: s.summary,
		Shows:   s.shows,
		Job:     s.jobSnapshotLocked(),
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// runRequest 是 /api/run 的请求体:由前端传回头尾(可能经过人工修改)。
type runRequest struct {
	Items []struct {
		Path string  `json:"path"`
		Head float64 `json:"head"`
		Tail float64 `json:"tail"`
	} `json:"items"`
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "读取请求失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req runRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "请求解析失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Items) == 0 {
		http.Error(w, "没有选中任何文件", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if s.job != nil && s.job.Running {
		s.mu.Unlock()
		http.Error(w, "已有任务正在执行", http.StatusConflict)
		return
	}

	// 把请求中的路径与探测结果对齐,构造 RunBatch 需要的输入。
	known := map[string]scan.Item{}
	for _, sh := range s.shows {
		for _, it := range sh.Items {
			known[it.Path] = scan.Item{
				Path: it.Path, Size: it.Size,
				Season: it.Season, Episode: it.Episode,
			}
		}
	}
	var items []scan.Item
	override := make(map[string]trim.HeadTail, len(req.Items))
	for _, ri := range req.Items {
		it, ok := known[ri.Path]
		if !ok {
			s.mu.Unlock()
			http.Error(w, "未知文件: "+ri.Path, http.StatusBadRequest)
			return
		}
		items = append(items, it)
		override[ri.Path] = trim.HeadTail{
			Head: secs(ri.Head),
			Tail: secs(ri.Tail),
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	states := make([]ItemStatus, 0, len(items))
	for _, it := range items {
		states = append(states, ItemStatus{Path: it.Path, Name: baseName(it.Path), Status: "pending"})
	}
	s.job = &JobState{
		Running: true, Started: time.Now(), Total: len(items), Items: states,
		BytesIn: totalSize(items),
	}
	s.mu.Unlock()

	go s.execute(ctx, items, override)

	writeJSON(w, map[string]any{"started": true, "total": len(items)})
}

// execute 在后台执行裁剪,通过 processResult 更新进度。
func (s *Server) execute(ctx context.Context, items []scan.Item, override map[string]trim.HeadTail) {
	// 每个文件开始处理时先标记为 running(按完成顺序推进)。
	batch := trim.RunBatch(ctx, s.ff, items, trim.BatchOptions{
		Trim:             s.opts,
		Workers:          s.workers,
		HeadTailOverride: override,
		OnResult:         s.onResult,
	})

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return
	}
	s.job.Running = false
	s.job.Finished = true
	s.job.Elapsed = time.Since(s.job.Started).Seconds()
	s.job.Done = batch.Done
	s.job.Failed = batch.Failed
	s.job.BytesOut = batch.TotalOut
	if ctx.Err() != nil {
		s.job.Canceled = true
		s.job.Error = "已取消"
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

// onResult 由 RunBatch 在每个文件完成后回调。
func (s *Server) onResult(r trim.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return
	}
	st := ItemStatus{
		Path:   r.Plan.Input,
		Name:   baseName(r.Plan.Input),
		Status: "done",
		Output: r.Output,
		Secs:   r.Elapsed.Seconds(),
	}
	if r.Err != nil {
		st.Status = "failed"
		st.Msg = r.Err.Error()
	}
	for i := range s.job.Items {
		if s.job.Items[i].Path == st.Path {
			s.job.Items[i] = st
			return
		}
	}
	s.job.Items = append(s.job.Items, st)
}

func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "仅支持 GET", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, s.jobSnapshotLocked())
}

// jobSnapshotLocked 生成当前任务快照(需已持有锁)。
func (s *Server) jobSnapshotLocked() *JobState {
	if s.job == nil {
		return nil
	}
	snap := *s.job
	if snap.Items != nil {
		snap.Items = append([]ItemStatus(nil), snap.Items...)
	}
	if snap.Running {
		snap.Elapsed = time.Since(snap.Started).Seconds()
	}
	// 补充尚未完成项的实时统计。
	done, failed := 0, 0
	for _, it := range snap.Items {
		switch it.Status {
		case "done":
			done++
		case "failed":
			failed++
		}
	}
	if done+failed > 0 {
		snap.Done, snap.Failed = done, failed
	}
	return &snap
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	cancel := s.cancel
	if cancel == nil && s.job != nil {
		// 任务已结束,仅标记。
		s.mu.Unlock()
		writeJSON(w, map[string]any{"canceled": false})
		return
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	writeJSON(w, map[string]any{"canceled": cancel != nil})
}

// shutdown 允许页面通知服务退出(配合 Ctrl+C 之外的使用方式)。
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST", http.StatusMethodNotAllowed)
		return
	}
	if s.onShutdown != nil {
		s.onShutdown()
	}
	writeJSON(w, map[string]any{"shutdown": true})
}

// Address 的实际监听地址通过 Listen 返回。

// Listen 在给定地址启动监听;addr 端口为 0 时由系统分配端口。
func (s *Server) Listen(addr string) (net.Listener, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		// 只有端口或只有主机名的情况都补成 host:port。
		addr = net.JoinHostPort(addr, "0")
	}
	return net.Listen("tcp", addr)
}

// Serve 阻塞服务请求,ctx 取消时优雅关闭。
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}

	// 保存 onShutdown 供 /api/shutdown 调用。
	stop := make(chan struct{})
	s.mu.Lock()
	s.onShutdown = func() { close(stop) }
	s.mu.Unlock()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case <-stop:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func secs(f float64) time.Duration {
	if f <= 0 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

func baseName(p string) string {
	i := lastSep(p)
	return p[i+1:]
}

func lastSep(p string) int {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return i
		}
	}
	return -1
}

func totalSize(items []scan.Item) int64 {
	var n int64
	for _, it := range items {
		n += it.Size
	}
	return n
}
