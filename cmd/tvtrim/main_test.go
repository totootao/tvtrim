package main

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"90", 90 * time.Second},
		{"90s", 90 * time.Second},
		{"90S", 90 * time.Second},
		{"1m", time.Minute},
		{"1m30s", 90 * time.Second},
		{"1h", time.Hour},
		{"1h2m3s", time.Hour + 2*time.Minute + 3*time.Second},
		{"2分钟", 2 * time.Minute},
		{"30秒", 30 * time.Second},
		{"1小时", time.Hour},
		{"1.5s", 1500 * time.Millisecond},
		{"0.5m", 30 * time.Second},
		{"1:30", 90 * time.Second},
		{"0:01:30", 90 * time.Second},
		{"1:00:00", time.Hour},
		{" 90 ", 90 * time.Second},
		{"1m 30s", 90 * time.Second},
	}

	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := parseDuration(c.in)
			if err != nil {
				t.Fatalf("parseDuration(%q) 报错: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("parseDuration(%q) = %v, 期望 %v", c.in, got, c.want)
			}
		})
	}
}

func TestParseDurationErrors(t *testing.T) {
	bad := []string{"abc", "1x", "1:2:3:4", "-5", "1:aa", "h", "1m30x"}
	for _, in := range bad {
		t.Run(in, func(t *testing.T) {
			if got, err := parseDuration(in); err == nil {
				t.Errorf("parseDuration(%q) 应报错,却返回 %v", in, got)
			}
		})
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45.00s"},
		{90 * time.Second, "1m30.0s"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1h02m03s"},
	}
	for _, c := range cases {
		if got := fmtDur(c.in); got != c.want {
			t.Errorf("fmtDur(%v) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{2048, "2.00 KB"},
		{1024 * 1024 * 5, "5.00 MB"},
		{int64(1024) * 1024 * 1024 * 3 / 2, "1.50 GB"},
	}
	for _, c := range cases {
		if got := humanSize(c.in); got != c.want {
			t.Errorf("humanSize(%d) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}
