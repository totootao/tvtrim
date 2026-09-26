package ffmpeg

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
)

// SilenceRange 是一段静音区间(单位:秒)。
type SilenceRange struct {
	Start    float64
	End      float64
	Duration float64
}

// silencedetect 输出形如:
//
//	[silencedetect @ 0x...] silence_start: 1.234
//	[silencedetect @ 0x...] silence_end: 3.456 | silence_duration: 2.222
var (
	reSilStart = regexp.MustCompile(`silence_start:\s*(-?[\d.]+)`)
	reSilEnd   = regexp.MustCompile(`silence_end:\s*(-?[\d.]+)\s*\|\s*silence_duration:\s*(-?[\d.]+)`)
)

// DetectSilences 用 silencedetect 滤镜检测输入文件中的静音区间。
//
// 只取第一条音频流做全片解码(纯音频解码,远快于整体转码);
// noiseDB 是静音判定阈值(如 -35 表示低于 -35dB 视为静音),
// minDur 是计入区间的最短静音秒数,totalSec 是文件总时长,
// 用于补齐"文件以静音收尾导致末尾没有 silence_end"的情况。
func (r *Runner) DetectSilences(ctx context.Context, input string, noiseDB, minDur, totalSec float64) ([]SilenceRange, error) {
	args := []string{
		"-i", input,
		"-map", "0:a:0",
		"-af", fmt.Sprintf("silencedetect=noise=%.0fdB:d=%.2f", noiseDB, minDur),
		"-f", "null", "-",
	}
	cmd := exec.CommandContext(ctx, r.Path, args...)
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	if r.Verbose {
		cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	} else {
		cmd.Stderr = &stderr
	}

	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if containsAny(msg, "matches no streams", "Stream map") {
			return nil, fmt.Errorf("该文件没有音轨,无法做静音检测")
		}
		return nil, fmt.Errorf("静音检测失败: %w", err)
	}
	return ParseSilences(stderr.String(), totalSec), nil
}

// ParseSilences 解析 silencedetect 的 stderr 输出。
// 末尾未闭合的 silence_start(文件以静音收尾)用 totalSec 补齐;
// totalSec <= 0 时丢弃该未闭合区间。
func ParseSilences(out string, totalSec float64) []SilenceRange {
	var (
		ranges []SilenceRange
		open   = -1.0 // 尚未闭合的 silence_start
	)
	for _, line := range splitLines(out) {
		if m := reSilEnd.FindStringSubmatch(line); m != nil && open >= 0 {
			end, _ := strconv.ParseFloat(m[1], 64)
			dur, _ := strconv.ParseFloat(m[2], 64)
			ranges = append(ranges, SilenceRange{Start: open, End: end, Duration: dur})
			open = -1
			continue
		}
		if m := reSilStart.FindStringSubmatch(line); m != nil {
			v, _ := strconv.ParseFloat(m[1], 64)
			// 同一时刻可能重复打印 start(极罕见),取最先出现的。
			if open < 0 {
				open = v
			}
		}
	}
	// 文件以静音收尾:有 start 没 end。
	if open >= 0 && totalSec > open {
		ranges = append(ranges, SilenceRange{
			Start: open, End: totalSec, Duration: totalSec - open,
		})
	}
	return ranges
}

// containsAny 判断 s 中是否包含任意一个子串。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && bytes.Contains([]byte(s), []byte(sub)) {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
