package testkit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFakeFFmpegProbeMode 验证"探测"调用会输出预设样本并以退出码 1 结束。
func TestFakeFFmpegProbeMode(t *testing.T) {
	dir := t.TempDir()
	sample := WriteFile(t, dir, "stderr.txt", "Duration: fake\n")
	SetEnv(t, EnvOut, sample)

	ff := WriteFakeFFmpeg(t, dir)
	var stderr strings.Builder
	cmd := exec.Command(ff, "-hide_banner", "-i", filepath.Join(dir, "in.mp4"))
	cmd.Stderr = &stderr
	err := cmd.Run()

	if err == nil {
		t.Fatal("探测模式应以非零退出码结束")
	}
	if got := stderr.String(); got != "Duration: fake\n" {
		t.Errorf("stderr = %q, 期望输出环境变量指向的样本", got)
	}
}

// TestFakeFFmpegTrimMode 验证"裁剪"调用会写出大于 1KB 的输出文件。
func TestFakeFFmpegTrimMode(t *testing.T) {
	dir := t.TempDir()
	ff := WriteFakeFFmpeg(t, dir)
	out := filepath.Join(dir, "out.mp4")

	cmd := exec.Command(ff, "-hide_banner", "-i", filepath.Join(dir, "in.mp4"),
		"-ss", "10", "-to", "100", "-c", "copy", "-f", "mp4", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("裁剪模式应以退出码 0 结束: %v\n%s", err, b)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatalf("输出文件未生成: %v", err)
	}
	if st.Size() != 5120 {
		t.Errorf("输出体积 = %d, 期望 5120", st.Size())
	}
}

// 失败/空输出两种异常模式。
func TestFakeFFmpegFailureModes(t *testing.T) {
	dir := t.TempDir()
	ff := WriteFakeFFmpeg(t, dir)

	t.Run("fail", func(t *testing.T) {
		SetEnv(t, EnvMode, ModeFail)
		out := filepath.Join(dir, "fail.mp4")
		cmd := exec.Command(ff, "-i", "in.mp4", "-ss", "1", "-f", "mp4", out)
		if err := cmd.Run(); err == nil {
			t.Error("fail 模式应以非零码退出")
		}
	})

	t.Run("empty", func(t *testing.T) {
		SetEnv(t, EnvMode, ModeEmpty)
		out := filepath.Join(dir, "empty.mp4")
		cmd := exec.Command(ff, "-i", "in.mp4", "-ss", "1", "-f", "mp4", out)
		if err := cmd.Run(); err != nil {
			t.Fatalf("empty 模式退出码应为 0: %v", err)
		}
		st, err := os.Stat(out)
		if err != nil {
			t.Fatalf("输出文件应存在: %v", err)
		}
		if st.Size() != 0 {
			t.Errorf("empty 模式应产出 0 字节,实际 %d", st.Size())
		}
	})
}

// TestSetEnvRestoresValue 确保 SetEnv 在所属测试结束后还原环境变量。
func TestSetEnvRestoresValue(t *testing.T) {
	const key = "TVTRIM_TEST_ENV"
	if err := os.Setenv(key, "before"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv(key)

	t.Run("内部设置", func(t *testing.T) {
		SetEnv(t, key, "during")
		if got := os.Getenv(key); got != "during" {
			t.Fatalf("设置后 = %q, 期望 during", got)
		}
	})

	// 子测试的 Cleanup 已执行完成,应回到调用前的值。
	if got := os.Getenv(key); got != "before" {
		t.Errorf("子测试结束后 = %q, 期望还原为 before", got)
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	p := WriteFile(t, dir, filepath.Join("sub", "a.txt"), "hello")
	if got := ReadFile(t, p); got != "hello" {
		t.Errorf("ReadFile = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub")); err != nil {
		t.Errorf("子目录应自动创建: %v", err)
	}
}
