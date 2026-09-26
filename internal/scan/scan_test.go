package scan

import "testing"

func TestParseEpisode(t *testing.T) {
	cases := []struct {
		name         string
		wantS, wantE int
	}{
		// SxxExx
		{"进击的巨人.S01E02.1080p.mkv", 1, 2},
		{"Show.S02E15.720p.WEB-DL.mp4", 2, 15},
		{"show.s1e5.avi", 1, 5},
		{"[Group] Show - S03E07 [1080p].mkv", 3, 7},
		{"S12E100.mp4", 12, 100},

		// 第N集 / 第N话
		{"某剧.第01集.mp4", 0, 1},
		{"某剧 第24集 1080p.mkv", 0, 24},
		{"动画 第100话.mp4", 0, 100},
		{"综艺 第3期.mp4", 0, 3},

		// EPxx / Exx
		{"Show.EP05.1080p.mp4", 0, 5},
		{"show-ep12.mkv", 0, 12},
		{"Show.E07.mkv", 0, 7},

		// 解析不到
		{"random_video.mp4", 0, 0},
		{"season.mp4", 0, 0},
		{"movie.mp4", 0, 0},
		{"", 0, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, e := ParseEpisode(c.name)
			if s != c.wantS || e != c.wantE {
				t.Errorf("ParseEpisode(%q) = (S%dE%d), 期望 (S%dE%d)",
					c.name, s, e, c.wantS, c.wantE)
			}
		})
	}
}

func TestParseEpisodeAvoidsFalsePositives(t *testing.T) {
	// "series" / "resource" / "release" 里的 s 或 e 不应被当成集号。
	for _, name := range []string{
		"series.mp4",
		"resource_2024.mp4",
		"released.mp4",
		"best_of.mp4",
	} {
		s, e := ParseEpisode(name)
		if s != 0 || e != 0 {
			t.Errorf("ParseEpisode(%q) = (S%dE%d), 期望 (S0E0)", name, s, e)
		}
	}
}

func TestParseEpisodePrefersSEPattern(t *testing.T) {
	// 同时含 SxxExx 和 EPxx 时,优先用 SxxExx。
	s, e := ParseEpisode("Show.S02E08.EP99.mkv")
	if s != 2 || e != 8 {
		t.Errorf("应优先解析 SxxExx,实际 (S%dE%d)", s, e)
	}
}
