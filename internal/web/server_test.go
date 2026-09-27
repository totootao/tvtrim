package web

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/totootao/tvtrim/internal/ffmpeg"
	"github.com/totootao/tvtrim/internal/scan"
	"github.com/totootao/tvtrim/internal/testkit"
	"github.com/totootao/tvtrim/internal/trim"
)

// probeStderr 是一份 20 分钟片子的 ffmpeg -i 输出样本。
const probeStderr = `ffmpeg version n6.0-trim Copyright (c) 2000-2023 the FFmpeg developers
Input #0, mov,mp4,m4a,3gp,3g2,mj2, from 'in.mp4':
  Metadata:
    major_brand     : isom
  Duration: 00:20:00.00, start: 0.000000, bitrate: 1843 kb/s
  Stream #0:0[0x1](und): Video: h264 (avc1 / 0x31637661), yuv420p, 1920x1080, 1700 kb/s, 25 fps
  Stream #0:1[0x2](und): Audio: aac (mp4a / 0x6134706D), 44100 Hz, stereo, fltp, 128 kb/s
At least one output file must be specified
`

// newFixture 造出一个可用的 Server:假 ffmpeg + 两集真实文件 + 已解析的 shows。
func newFixture(t *testing.T) (*Server, []string) {
	t.Helper()
	root := t.TempDir()
	ffPath := testkit.WriteFakeFFmpeg(t, root)
	testkit.SetEnv(t, testkit.EnvOut, testkit.WriteFile(t, root, "probe.txt", probeStderr))

	dir := filepath.Join(root, "剧集A")
	names := []string{"S01E01.mp4", "S01E02.mp4"}
	var paths []string
	for _, n := range names {
		paths = append(paths, testkit.WriteFile(t, dir, n, strings.Repeat("a", 4096)))
	}

	var items []scan.Item
	for i, p := range paths {
		items = append(items, scan.Item{Path: p, Size: 4096, Season: 1, Episode: i + 1})
	}
	// 手动模式:统一 head=30s / tail=40s,两集都可执行。
	shows := BuildShows(items, nil, nil, 30, 40)

	opts := trim.Options{
		Head:      30 * time.Second,
		Tail:      40 * time.Second,
		Suffix:    trim.DefaultSuffix,
		Overwrite: true,
	}
	srv := New(&ffmpeg.Runner{Path: ffPath}, shows, opts, 2, "test-version", ModeManual)
	return srv, paths
}

// do 用 httptest 驱动请求,返回状态码与响应体。
func do(t *testing.T, srv *Server, method, target, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func decode[T any](t *testing.T, raw string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("解析 JSON 失败: %v (原文: %s)", err, raw)
	}
	return v
}

func TestHandleIndex(t *testing.T) {
	srv, _ := newFixture(t)
	code, body := do(t, srv, http.MethodGet, "/", "")
	if code != http.StatusOK {
		t.Fatalf("GET / 状态码 = %d", code)
	}
	if !strings.Contains(body, "tvtrim") {
		t.Error("首页内容异常:未包含 tvtrim 关键字")
	}
}

func TestHandleIndexNotFound(t *testing.T) {
	srv, _ := newFixture(t)
	if code, _ := do(t, srv, http.MethodGet, "/other", ""); code != http.StatusNotFound {
		t.Errorf("未知路径状态码 = %d, 期望 404", code)
	}
}

func TestHandleState(t *testing.T) {
	srv, _ := newFixture(t)
	code, body := do(t, srv, http.MethodGet, "/api/state", "")
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d", code)
	}
	st := decode[stateResponse](t, body)
	if st.Version != "test-version" {
		t.Errorf("version = %q", st.Version)
	}
	if st.Mode != ModeManual {
		t.Errorf("mode = %q, 期望 manual", st.Mode)
	}
	if st.Summary.Shows != 1 || st.Summary.Items != 2 || st.Summary.Ready != 2 {
		t.Errorf("summary 不符: %+v", st.Summary)
	}
	if len(st.Shows) != 1 || len(st.Shows[0].Items) != 2 {
		t.Fatalf("shows 结构不符: %+v", st.Shows)
	}
	if st.Job != nil {
		t.Error("尚未执行任务时 job 应为 nil")
	}
	if st.FFmpeg == "" {
		t.Error("应回显 ffmpeg 路径")
	}
}

func TestAPIRejectsWrongMethod(t *testing.T) {
	srv, _ := newFixture(t)
	cases := []struct {
		path, want string
	}{
		{"/api/state", http.MethodGet},
		{"/api/run", http.MethodPost},
		{"/api/progress", http.MethodGet},
		{"/api/cancel", http.MethodPost},
		{"/api/shutdown", http.MethodPost},
	}
	for _, c := range cases {
		wrong := http.MethodGet
		if c.want == http.MethodGet {
			wrong = http.MethodPost
		}
		code, _ := do(t, srv, wrong, c.path, "")
		if code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s 状态码 = %d, 期望 405", wrong, c.path, code)
		}
	}
}

func TestHandleRunRejectsEmptyAndUnknown(t *testing.T) {
	srv, paths := newFixture(t)

	code, _ := do(t, srv, http.MethodPost, "/api/run", `{"items":[]}`)
	if code != http.StatusBadRequest {
		t.Errorf("空列表状态码 = %d, 期望 400", code)
	}

	code, body := do(t, srv, http.MethodPost, "/api/run",
		`{"items":[{"path":"/nope/x.mp4","head":1,"tail":1}]}`)
	if code != http.StatusBadRequest {
		t.Errorf("未知文件状态码 = %d, 期望 400", code)
	}
	if !strings.Contains(body, "未知文件") {
		t.Errorf("错误信息不符: %s", body)
	}
	_ = paths
}

func TestHandleRunBadJSON(t *testing.T) {
	srv, _ := newFixture(t)
	if code, _ := do(t, srv, http.MethodPost, "/api/run", "{oops"); code != http.StatusBadRequest {
		t.Errorf("非法 JSON 状态码 = %d, 期望 400", code)
	}
}

// 已有任务在跑时不允许再提交,返回 409。
func TestHandleRunConflict(t *testing.T) {
	srv, paths := newFixture(t)
	srv.job = &JobState{Running: true, Total: 1, Items: []ItemStatus{{Path: paths[0], Status: "running"}}}

	code, body := do(t, srv, http.MethodPost, "/api/run",
		`{"items":[{"path":"`+paths[0]+`","head":30,"tail":40}]}`)
	if code != http.StatusConflict {
		t.Errorf("冲突状态码 = %d, 期望 409", code)
	}
	if !strings.Contains(body, "已有任务") {
		t.Errorf("错误信息不符: %s", body)
	}
}

// 端到端:提交任务 → 轮询进度 → 校验输出文件确实生成且人工改的 head 生效。
func TestHandleRunExecutesAndReportsProgress(t *testing.T) {
	srv, paths := newFixture(t)

	body, err := json.Marshal(map[string]any{"items": []map[string]any{
		{"path": paths[0], "head": 30, "tail": 40},
		{"path": paths[1], "head": 10, "tail": 20}, // 人工把片头从 30s 调到 10s
	}})
	if err != nil {
		t.Fatal(err)
	}
	code, raw := do(t, srv, http.MethodPost, "/api/run", string(body))
	if code != http.StatusOK {
		t.Fatalf("提交任务状态码 = %d: %s", code, raw)
	}
	started := decode[map[string]any](t, raw)
	if started["started"] != true || started["total"] != float64(2) {
		t.Errorf("run 响应不符: %+v", started)
	}

	// 轮询直到任务结束。
	var job *JobState
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code, raw = do(t, srv, http.MethodGet, "/api/progress", "")
		if code != http.StatusOK {
			t.Fatalf("进度接口状态码 = %d", code)
		}
		job = decode[*JobState](t, raw)
		if !job.Running {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job == nil {
		t.Fatal("未拿到任务快照")
	}
	if job.Running {
		t.Fatal("任务未在超时前结束")
	}
	if !job.Finished {
		t.Error("finished 应为 true")
	}
	if job.Done != 2 || job.Failed != 0 {
		t.Errorf("done=%d failed=%d, 期望 2/0 (明细: %+v)", job.Done, job.Failed, job.Items)
	}
	if len(job.Items) != 2 {
		t.Fatalf("明细项数 = %d, 期望 2", len(job.Items))
	}
	for _, it := range job.Items {
		if it.Status != "done" {
			t.Errorf("%s 状态 = %s, msg=%s", it.Name, it.Status, it.Msg)
		}
		if !strings.Contains(it.Output, "-trim") {
			t.Errorf("%s 输出路径异常: %q", it.Name, it.Output)
		}
		if _, err := os.Stat(it.Output); err != nil {
			t.Errorf("输出文件不存在: %v", err)
		}
	}
	if job.BytesOut <= 0 {
		t.Errorf("汇总输出字节数 = %d, 期望 > 0", job.BytesOut)
	}
	if job.Elapsed <= 0 {
		t.Errorf("耗时 = %v, 期望 > 0", job.Elapsed)
	}

	// 已结束的任务快照也应出现在 /api/state 里,方便刷新页面后看到结果。
	_, raw = do(t, srv, http.MethodGet, "/api/state", "")
	st := decode[stateResponse](t, raw)
	if st.Job == nil || st.Job.Done != 2 {
		t.Errorf("state 里的 job 快照不符: %+v", st.Job)
	}
}

// 输出文件已存在且未开 overwrite 时,该集应失败并在进度里给出原因。
func TestRunFailureIsReported(t *testing.T) {
	srv, paths := newFixture(t)
	srv.opts.Overwrite = false
	// 预先占位,触发"输出文件已存在"。
	testkit.WriteFile(t, filepath.Dir(paths[0]), "S01E01-trim.mp4", strings.Repeat("x", 2048))

	do(t, srv, http.MethodPost, "/api/run",
		`{"items":[{"path":"`+paths[0]+`","head":30,"tail":40}]}`)

	deadline := time.Now().Add(10 * time.Second)
	var job *JobState
	for time.Now().Before(deadline) {
		_, raw := do(t, srv, http.MethodGet, "/api/progress", "")
		job = decode[*JobState](t, raw)
		if !job.Running {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.Done != 0 || job.Failed != 1 {
		t.Errorf("done=%d failed=%d, 期望 0/1", job.Done, job.Failed)
	}
	if len(job.Items) != 1 || job.Items[0].Status != "failed" {
		t.Fatalf("失败项状态不符: %+v", job.Items)
	}
	if !strings.Contains(job.Items[0].Msg, "输出文件已存在") {
		t.Errorf("失败原因不符: %q", job.Items[0].Msg)
	}
}

// 取消:任务进行中返回 canceled=true;没有任务时返回 false。
func TestHandleCancel(t *testing.T) {
	srv, _ := newFixture(t)

	if code, _ := do(t, srv, http.MethodPost, "/api/cancel", ""); code != http.StatusOK {
		t.Fatal("无任务时 cancel 也应正常返回")
	}
	_, raw := do(t, srv, http.MethodPost, "/api/cancel", "")
	res := decode[map[string]any](t, raw)
	if res["canceled"] != false {
		t.Errorf("无任务时 canceled = %v, 期望 false", res["canceled"])
	}

	srv.job = &JobState{Running: true}
	srv.cancel = func() {}
	_, raw = do(t, srv, http.MethodPost, "/api/cancel", "")
	res = decode[map[string]any](t, raw)
	if res["canceled"] != true {
		t.Errorf("执行中 canceled = %v, 期望 true", res["canceled"])
	}
}

func TestJobSnapshotRunningUpdatesElapsed(t *testing.T) {
	srv, _ := newFixture(t)
	srv.job = &JobState{Running: true, Started: time.Now().Add(-2 * time.Second), Total: 3}
	snap := srv.jobSnapshotLocked()
	if snap == nil {
		t.Fatal("快照不应为 nil")
	}
	if snap.Elapsed < 1.9 {
		t.Errorf("运行中的 elapsed = %v, 期望约 2s", snap.Elapsed)
	}
	// 快照必须是副本,修改它不能影响原状态。
	snap.Done = 99
	if srv.job.Done != 0 {
		t.Error("jobSnapshotLocked 返回了共享引用")
	}
}

// onResult 遇到未知文件时应追加而不是修改既有项。
func TestOnResultAppendsUnknownItem(t *testing.T) {
	srv, _ := newFixture(t)
	srv.job = &JobState{Running: true, Items: []ItemStatus{{Path: "/known.mp4", Status: "pending"}}}
	srv.onResult(trim.Result{
		Plan:   &trim.Plan{Input: "/other.mp4"},
		Err:    io.ErrUnexpectedEOF,
		Output: "",
	})
	if len(srv.job.Items) != 2 {
		t.Fatalf("明细项数 = %d, 期望 2", len(srv.job.Items))
	}
	if srv.job.Items[1].Status != "failed" || srv.job.Items[1].Name != "other.mp4" {
		t.Errorf("追加项不符: %+v", srv.job.Items[1])
	}
}

// onResult 在 job 为空时应静默返回(避免 httptest 之外的场景 panic)。
func TestOnResultWithoutJob(t *testing.T) {
	srv, _ := newFixture(t)
	srv.onResult(trim.Result{Plan: &trim.Plan{Input: "/x.mp4"}})
}

func TestHandleShutdownInvokesCallback(t *testing.T) {
	srv, _ := newFixture(t)
	called := false
	srv.onShutdown = func() { called = true }
	code, raw := do(t, srv, http.MethodPost, "/api/shutdown", "")
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d", code)
	}
	res := decode[map[string]any](t, raw)
	if res["shutdown"] != true || !called {
		t.Errorf("关闭回调未被触发: %+v called=%v", res, called)
	}
}

func TestListenAllocatesPort(t *testing.T) {
	srv, _ := newFixture(t)
	cases := []string{"", "127.0.0.1:0", "127.0.0.1"}
	for _, addr := range cases {
		ln, err := srv.Listen(addr)
		if err != nil {
			t.Fatalf("Listen(%q) 失败: %v", addr, err)
		}
		if ln.Addr() == nil {
			t.Errorf("Listen(%q) 地址为空", addr)
		}
		_ = ln.Close()
	}
}

// Serve 能通过 context 取消退出。
func TestServeStopsOnContextCancel(t *testing.T) {
	srv, _ := newFixture(t)
	ln, err := srv.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx, ln) }()

	// 先确认服务真的起来了。
	base := "http://" + ln.Addr().String()
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/state")
		if err == nil {
			resp.Body.Close()
			lastErr = nil
			break
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("服务未就绪: %v", lastErr)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("优雅退出报错: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve 未在 5s 内退出")
	}
}

// Serve 也能被 /api/shutdown 触发退出。
func TestServeStopsOnShutdownAPI(t *testing.T) {
	srv, _ := newFixture(t)
	ln, err := srv.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(context.Background(), ln) }()

	base := "http://" + ln.Addr().String()
	resp, err := http.Post(base+"/api/shutdown", "application/json", nil)
	if err != nil {
		t.Fatalf("调用 shutdown 失败: %v", err)
	}
	resp.Body.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("退出报错: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve 未在 5s 内退出")
	}
}

// TestListenWildcardUsesIPv4 监听 0.0.0.0 时必须拿到 IPv4 socket。
// Go 默认会把它建成 IPv6 双栈(netstat 显示 :::port),在禁用 IPv6 的环境里
// docker -p 转发过来的 IPv4 连接会落空,表现为"容器在跑但连不上"。
func TestListenWildcardUsesIPv4(t *testing.T) {
	ln, err := (&Server{}).Listen("0.0.0.0:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()

	host, _, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("解析监听地址失败: %v", err)
	}
	if ip := net.ParseIP(host); ip == nil || ip.To4() == nil {
		t.Errorf("应监听 IPv4 地址, 实际 %q", ln.Addr())
	}
}

// TestListenLoopbackStillWorks 回环地址的行为不变(端口 0 由系统分配)。
func TestListenLoopbackStillWorks(t *testing.T) {
	ln, err := (&Server{}).Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()
	if !strings.HasPrefix(ln.Addr().String(), "127.0.0.1:") {
		t.Errorf("回环监听地址 = %q", ln.Addr())
	}
}

// TestListenPortOnly 只给端口号时补成 host:port,不该报错。
func TestListenPortOnly(t *testing.T) {
	ln, err := (&Server{}).Listen("0")
	if err != nil {
		t.Fatalf("只给端口也应能监听: %v", err)
	}
	ln.Close()
}
