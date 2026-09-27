package scan

import (
	"path/filepath"
	"regexp"
	"strings"
)

// 一个目录里常常混排多部剧,只看目录名无法把它们拆开。
// 本文件从文件名里解析剧名,让分组与纠错都能"按剧"进行。

// tagRe 匹配 [字幕组] / (1080p) / 【xx】 这类括号标签。
var tagRe = regexp.MustCompile(`[\[(【][^])】]*[)】\]]`)

// yearRe 匹配剧名尾部的年份(如 "The Show 2019")。
var yearRe = regexp.MustCompile(`(?i)[\s._-]*(19|20)\d{2}\s*$`)

// junkTokens 是剧名尾巴上常见的分辨率/片源/编码标签,去掉后剧名更干净。
var junkTokens = map[string]bool{
	"1080p": true, "720p": true, "480p": true, "2160p": true, "1440p": true,
	"4k": true, "8k": true, "hdr": true, "hdr10": true, "sdr": true,
	"web": true, "web-dl": true, "webrip": true, "bluray": true, "bdrip": true,
	"hdtv": true, "dvdrip": true, "remux": true,
	"x264": true, "x265": true, "h264": true, "h265": true, "hevc": true,
	"aac": true, "ac3": true, "flac": true, "10bit": true,
	"chs": true, "cht": true, "gb": true, "big5": true,
	"中字": true, "双语": true, "国语": true, "内嵌字幕": true,
}

// ShowOf 从文件名解析剧名,解析不到返回空串(由调用方回退到目录名)。
//
// 思路:先剥掉括号标签,再找到第一个"季集标记"(S01E02 / 第01集 / EP02)的
// 位置,它前面的部分就是剧名;最后清洗分隔符与尾部技术标签。
func ShowOf(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	base = tagRe.ReplaceAllString(base, "")

	idx := indexEpisodeMark(base)
	if idx <= 0 {
		return ""
	}
	return CleanShow(base[:idx])
}

// indexEpisodeMark 返回文件名中第一个季集标记的下标,找不到返回 -1。
// 顺序与 ParseEpisode 一致:SxxExx 优先,其次"第N集",最后 EPxx/E xx。
func indexEpisodeMark(s string) int {
	lower := strings.ToLower(s)

	// SxxExx:s 与 e 之间必须有数字,且 s 前面不是数字(避开 resource 之类)。
	for i := 0; i+1 < len(lower); i++ {
		if lower[i] != 's' {
			continue
		}
		if i > 0 && isDigit(lower[i-1]) {
			continue
		}
		j := i + 1
		for j < len(lower) && isDigit(lower[j]) {
			j++
		}
		if j == i+1 || j >= len(lower) || lower[j] != 'e' {
			continue
		}
		k := j + 1
		for k < len(lower) && isDigit(lower[k]) {
			k++
		}
		if k == j+1 {
			continue
		}
		return i
	}

	// 第N集 / 第N话。
	if i := strings.Index(s, "第"); i >= 0 {
		j := i + len("第")
		k := j
		for k < len(s) && isDigit(s[k]) {
			k++
		}
		if k > j && k < len(s) {
			next := string([]rune(s[k:])[0])
			if next == "集" || next == "话" || next == "期" || next == "部" {
				return i
			}
		}
	}

	// EP02 / E02(必须处于词首)。
	for _, p := range []string{"ep", "e"} {
		i := strings.Index(lower, p)
		if i < 0 {
			continue
		}
		if i > 0 {
			c := lower[i-1]
			if isDigit(c) || (c >= 'a' && c <= 'z') {
				continue
			}
		}
		j := i + len(p)
		k := j
		for k < len(lower) && isDigit(lower[k]) {
			k++
		}
		if k > j {
			return i
		}
	}
	return -1
}

// CleanShow 清洗剧名:统一分隔符、去掉年份与尾部技术标签。
func CleanShow(title string) string {
	s := tagRe.ReplaceAllString(title, "")
	// 剧名里常见的点/下划线其实是分隔符(英文剧名几乎不用点做标点)。
	s = strings.NewReplacer(".", " ", "_", " ", "+", " ").Replace(s)
	s = yearRe.ReplaceAllString(s, "")
	// 去掉尾部分隔符与连字符("Show -" / "Show_")。
	s = strings.TrimRight(strings.TrimSpace(s), "-–—_ .")
	// 压缩连续空白。
	s = strings.Join(strings.Fields(s), " ")
	// 逐轮去掉尾部的技术标签("Show 1080p WEB-DL")。
	for {
		fields := strings.Fields(s)
		if len(fields) == 0 {
			return ""
		}
		last := strings.ToLower(fields[len(fields)-1])
		if junkTokens[last] {
			s = strings.Join(fields[:len(fields)-1], " ")
			s = strings.TrimRight(strings.TrimSpace(s), "-–—_ .")
			continue
		}
		return strings.TrimSpace(s)
	}
}

// ShowKey 把剧名归一化成分组用的键:忽略大小写与分隔符差异,
// 这样 "Show.Name" 与 "Show-Name" 会被当成同一部剧。
func ShowKey(show string) string {
	s := strings.ToLower(strings.TrimSpace(show))
	var b strings.Builder
	for _, r := range s {
		if r == ' ' || r == '.' || r == '_' || r == '-' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// CountShows 统计扫描结果里有明确剧名的剧数(用于 CLI 概览输出)。
// 无法解析出剧名的文件按所在目录各自算一部。
func CountShows(items []Item) int {
	seen := map[string]bool{}
	for _, it := range items {
		key := ShowKey(it.Show)
		if key == "" {
			key = "@dir:" + filepath.Dir(it.Path)
		}
		seen[key] = true
	}
	return len(seen)
}
