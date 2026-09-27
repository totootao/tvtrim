// Package testkit 提供测试用的辅助设施:
// 一个"假 ffmpeg"可执行脚本,用于在不需要真实 ffmpeg 二进制的情况下
// 驱动 Probe / DetectSilences / RunBatch 等真实调用路径。
//
// 之所以不用 mock 接口而是造真进程:被测代码大量依赖子进程退出码、
// stderr 文本与文件副作用,用脚本模拟比抽象一层接口更贴近真实行为。
package testkit

import (
	"os"
	"path/filepath"
	"testing"
)

// EnvMode / EnvOut 是控制假 ffmpeg 行为的环境变量。
const (
	EnvMode = "TVTRIM_FAKE_MODE"
	EnvOut  = "TVTRIM_FAKE_OUT"
	// EnvKeyDTS 指定假 ffmpeg 在 -debug_ts 模式下报告的首个视频包 DTS(秒)。
	// 用于验证输出侧 -ss 回溯到关键帧时的时间戳补偿逻辑。
	EnvKeyDTS = "TVTRIM_FAKE_KEY_DTS"
	// EnvHasVideo 为 "0" 时让 -debug_ts 模式不输出视频行(模拟纯音频文件)。
	EnvHasVideo = "TVTRIM_FAKE_HAS_VIDEO"
)

// Fake 行为的取值。
const (
	ModeProbe   = "probe"   // 输出预设 stderr 并以退出码 1 结束(同 `ffmpeg -i file`)
	ModeSilence = "silence" // 输出预设 stderr 并成功退出(silencedetect)
	ModeCopy    = "copy"    // 创建输出文件(模拟 stream copy 成功)
	ModeFail    = "fail"    // 输出错误并以退出码 1 结束
	ModeEmpty   = "empty"   // 创建小于 1KB 的空输出文件(触发空输出检测)
	// ModeNoSilence 让探测正常、但静音检测失败(退出 1 且无输出)。
	// 用于区分"结论来自缓存"与"结论来自本次检测":命中缓存的文件照样有结论。
	ModeNoSilence = "nosilence"
)

// FakeScript 是假 ffmpeg 的实现。
//
// 行为按参数自动判别,便于在同一个进程里先后支撑探测与裁剪:
//   - 含 `-af …silencedetect` → 输出 TVTRIM_FAKE_OUT 指向的文本,退出 0
//   - 含 `-ss`/`-to`/`-c copy` → 写出输出文件(>1KB,避免被空输出检测拦下),退出 0
//   - 其余(通常是 `ffmpeg -i x`) → 输出 TVTRIM_FAKE_OUT 指向的文本,退出 1
//
// TVTRIM_FAKE_MODE 为 empty/fail 时覆盖写文件行为,用于验证异常分支;
// 为 nosilence 时静音检测直接失败,用于验证"沿用缓存"这条路径。
const FakeScript = `#!/usr/bin/env bash
out="${TVTRIM_FAKE_OUT}"
force="${TVTRIM_FAKE_MODE}"

silence=0
trim=0
debugts=0
for a in "$@"; do
  case "$a" in
    silencedetect*) silence=1 ;;
    -ss|-to|-f) trim=1 ;;
    -debug_ts) debugts=1 ;;
  esac
done

emit() { [ -n "$out" ] && [ -f "$out" ] && cat "$out" >&2; }

# -debug_ts:输出首个视频包的 demuxer 时间戳,供关键帧探测使用。
# 默认报告 DTS=0.000(相当于 -ss 正好落在关键帧上,无需补偿)。
if [ "$debugts" = "1" ]; then
  if [ "${TVTRIM_FAKE_HAS_VIDEO:-1}" = "0" ]; then
    echo "[vist#0:0/aac @ 0x0] demuxer -> ist_index:0:1 type:audio pkt_pts:0 pkt_pts_time:0.000 pkt_dts:0 pkt_dts_time:0.000" >&2
    exit 0
  fi
  dts="${TVTRIM_FAKE_KEY_DTS:-0.000}"
  echo "[vist#0:0/hevc @ 0x0] demuxer -> ist_index:0:0 type:video pkt_pts:0 pkt_pts_time:${dts} pkt_dts:0 pkt_dts_time:${dts} duration:3600 duration_time:0.04" >&2
  echo "[vist#0:0/hevc @ 0x0] demuxer+tsfixup -> ist_index:0:0 type:video pkt_pts:0 pkt_pts_time:0.000 pkt_dts:0 pkt_dts_time:0.000" >&2
  exit 0
fi

if [ "$silence" = "1" ]; then
  if [ "$force" = "nosilence" ]; then exit 1; fi
  emit
  exit 0
fi

if [ "$trim" = "1" ]; then
  dst="${!#}"
  case "$dst" in -*) dst="out.bin" ;; esac
  if [ "$force" = "empty" ]; then : > "$dst"; exit 0; fi
  if [ "$force" = "fail" ]; then
    echo "Error opening output file $dst." >&2
    echo "Invalid argument" >&2
    exit 1
  fi
  head -c 5120 /dev/zero > "$dst"
  exit 0
fi

emit
exit 1
`

// WriteFakeFFmpeg 在 dir 下写入可执行的假 ffmpeg,返回其路径。
func WriteFakeFFmpeg(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-ffmpeg")
	if err := os.WriteFile(path, []byte(FakeScript), 0o755); err != nil {
		t.Fatalf("写入假 ffmpeg 失败: %v", err)
	}
	return path
}

// WriteFile 在 dir 下写入文件并返回路径,父目录不存在时自动创建。
func WriteFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}
	return path
}

// ReadFile 读取文件内容。
func ReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(b)
}

// SetEnv 设置环境变量并返回还原函数。
func SetEnv(t *testing.T, key, value string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	if err := os.Setenv(key, value); err != nil {
		t.Fatalf("设置 %s 失败: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv(key, old)
		} else {
			os.Unsetenv(key)
		}
	})
}
