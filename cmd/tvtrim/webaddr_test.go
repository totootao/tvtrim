package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExternalURLHint(t *testing.T) {
	cases := []struct {
		addr    string
		wantHit bool
	}{
		{"0.0.0.0:8080", true},
		{"[::]:8080", true},
		{"127.0.0.1:34567", false},
		{"127.0.0.1:0", false},
		{"localhost", false}, // 缺端口,SplitHostPort 报错
		{"", false},
	}
	for _, c := range cases {
		hint := externalURLHint(c.addr)
		if (hint != "") != c.wantHit {
			t.Errorf("externalURLHint(%q) = %q, 有提示应为 %v", c.addr, hint, c.wantHit)
		}
		if c.wantHit && !strings.Contains(hint, "docker -p") {
			t.Errorf("externalURLHint(%q) 未说明 docker -p 映射: %q", c.addr, hint)
		}
	}
}

func TestExternalURLHintMentionsPort(t *testing.T) {
	hint := externalURLHint("0.0.0.0:8080")
	if !strings.Contains(hint, "8080") {
		t.Errorf("提示里应带上容器端口 8080: %q", hint)
	}
}

// fakeEnv 构造一个只认识若干键的取值函数。
func fakeEnv(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestResolveWebAddrUsesContainerDefault(t *testing.T) {
	t.Setenv("TVTRIM_CONTAINER", "1")
	if got := resolveWebAddr(""); got != containerWebAddr {
		t.Errorf("容器内默认地址 = %q, 需要 %q", got, containerWebAddr)
	}
}

func TestResolveWebAddrUsesLocalDefault(t *testing.T) {
	t.Setenv("TVTRIM_CONTAINER", "0")
	if got := resolveWebAddr(""); got != localWebAddr {
		t.Errorf("宿机默认地址 = %q, 需要 %q", got, localWebAddr)
	}
}

// TestResolveWebAddrExplicitWins 保证命令行显式指定的地址不被环境探测覆盖。
func TestResolveWebAddrExplicitWins(t *testing.T) {
	for _, env := range []string{"1", "0"} {
		t.Setenv("TVTRIM_CONTAINER", env)
		if got := resolveWebAddr("127.0.0.1:34567"); got != "127.0.0.1:34567" {
			t.Errorf("TVTRIM_CONTAINER=%s 时显式地址被改写为 %q", env, got)
		}
	}
}

func TestLooksContainerEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".dockerenv")
	if err := os.WriteFile(marker, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		val  string
		want bool
	}{
		{"1", true}, {"true", true}, {"TRUE", true}, {"yes", true}, {"on", true},
		{"0", false}, {"false", false}, {"no", false}, {"off", false},
	}
	for _, c := range cases {
		if got := looksContainer(marker, "", fakeEnv(map[string]string{"TVTRIM_CONTAINER": c.val})); got != c.want {
			t.Errorf("TVTRIM_CONTAINER=%q → %v, 需要 %v", c.val, got, c.want)
		}
	}
}

// TestLooksContainerEnvFalseBeatsDockerEnv 验证显式关闭优先于 /.dockerenv 探测。
func TestLooksContainerEnvFalseBeatsDockerEnv(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".dockerenv")
	if err := os.WriteFile(marker, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	got := looksContainer(marker, "", fakeEnv(map[string]string{"TVTRIM_CONTAINER": "0"}))
	if got {
		t.Error("TVTRIM_CONTAINER=0 时不该因为 /.dockerenv 存在而判定为容器")
	}
}

func TestLooksContainerByDockerEnvFile(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".dockerenv")
	if err := os.WriteFile(marker, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	if !looksContainer(marker, filepath.Join(dir, "no-such-cgroup"), fakeEnv(nil)) {
		t.Error("存在 /.dockerenv 时应判定为容器")
	}
}

func TestLooksContainerByCgroup(t *testing.T) {
	dir := t.TempDir()
	cgroup := filepath.Join(dir, "cgroup")
	bodies := []string{
		"0::/\n",
		"12:pids:/docker/3d3a9f1c\n",
		"11:cpuset:/kubepods/burstable/pod123\n",
		"1:name=systemd:/system.slice/containerd-abc.scope\n",
		"1:name=systemd:/system.slice/crio-abc.scope\n",
	}
	for _, body := range bodies {
		if err := os.WriteFile(cgroup, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		got := looksContainer(filepath.Join(dir, "no-such-dockerenv"), cgroup, fakeEnv(nil))
		want := body != "0::/\n"
		if got != want {
			t.Errorf("cgroup=%q → %v, 需要 %v", body, got, want)
		}
	}
}

func TestLooksContainerBareMetal(t *testing.T) {
	dir := t.TempDir()
	got := looksContainer(filepath.Join(dir, "no-such-dockerenv"), filepath.Join(dir, "no-such-cgroup"), fakeEnv(nil))
	if got {
		t.Error("既无 /.dockerenv 也无容器 cgroup 标记时不应判定为容器")
	}
}

// TestLooksContainerIgnoresUnknownEnvValue 覆盖环境变量取到垃圾值的分支:
// 既不匹配真值也不匹配假值时,应继续走文件探测。
func TestLooksContainerIgnoresUnknownEnvValue(t *testing.T) {
	dir := t.TempDir()
	if got := looksContainer(
		filepath.Join(dir, "none"), filepath.Join(dir, "none"),
		fakeEnv(map[string]string{"TVTRIM_CONTAINER": "maybe"})); got {
		t.Error("无法识别的环境变量值应忽略,而非判定为容器")
	}
}
