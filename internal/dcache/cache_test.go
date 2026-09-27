package dcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadMissingIsEmpty 首次运行没有缓存文件,不该报错。
func TestLoadMissingIsEmpty(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("缓存不存在时应返回空缓存, 实际报错: %v", err)
	}
	if len(s.Files) != 0 {
		t.Errorf("空缓存应为 0 条, 实际 %d", len(s.Files))
	}
}

// TestRoundTrip 写入再读回,结论与校验字段都要保持一致。
func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.mkv")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "cache.json")
	s := New(Params{NoiseDB: -35, MinSilence: 0.8, Sample: 3}.Signature())
	s.Put(file, info.Size(), info.ModTime(), Entry{
		Head: 30, Tail: 45, OK: true, Note: "片头切点 30.0s", Show: "秀", Season: 1,
	})
	if err := s.Save(path); err != nil {
		t.Fatalf("保存缓存失败: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("读取缓存失败: %v", err)
	}
	if !got.Usable(s.Params) {
		t.Fatalf("同一组参数下缓存应可用: params=%q", got.Params)
	}
	e, ok := got.Lookup(file, info.Size())
	if !ok {
		t.Fatal("同一文件应命中缓存")
	}
	if e.Head != 30 || e.Tail != 45 || !e.OK {
		t.Errorf("切点不一致: %+v", e)
	}
	if e.Show != "秀" || e.Season != 1 {
		t.Errorf("剧名/季号未保留: %+v", e)
	}
}

// TestLookupDetectsChangedFile 文件被替换后缓存必须失效,否则会拿旧的切点去剪新的片。
func TestLookupDetectsChangedFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.mkv")
	os.WriteFile(file, []byte("old"), 0o644)
	info, _ := os.Stat(file)

	s := New("p")
	s.Put(file, info.Size(), info.ModTime(), Entry{Head: 30, Tail: 45, OK: true})

	if _, ok := s.Lookup(file, info.Size()); !ok {
		t.Fatal("未改动时应命中")
	}

	// 内容变了:大小与 mtime 都会变。
	os.WriteFile(file, []byte("much longer content"), 0o644)
	if _, ok := s.Lookup(file, info.Size()); ok {
		t.Error("大小变了却仍命中缓存")
	}
	newInfo, _ := os.Stat(file)
	if _, ok := s.Lookup(file, newInfo.Size()); ok {
		t.Error("mtime 变了却仍命中缓存")
	}

	// 文件被删掉。
	os.Remove(file)
	if _, ok := s.Lookup(file, 100); ok {
		t.Error("文件已不存在却仍命中缓存")
	}
}

// TestLookupSkipsFailed 识别失败的记录不该被当成结论复用。
func TestLookupSkipsFailed(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.mkv")
	os.WriteFile(file, []byte("x"), 0o644)
	info, _ := os.Stat(file)

	s := New("p")
	s.Put(file, info.Size(), info.ModTime(), Entry{OK: false})
	if _, ok := s.Lookup(file, info.Size()); ok {
		t.Error("失败记录不该命中")
	}
}

// TestParamsInvalidate 换参数(阈值/抽样数/结构版本)后整份缓存失效。
func TestParamsInvalidate(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.mkv")
	os.WriteFile(file, []byte("x"), 0o644)
	info, _ := os.Stat(file)

	s := New(Params{NoiseDB: -35, MinSilence: 0.8, Sample: 3}.Signature())
	s.Put(file, info.Size(), info.ModTime(), Entry{Head: 30, Tail: 45, OK: true})

	other := Params{NoiseDB: -35, MinSilence: 0.8, Sample: 0}.Signature()
	if other == s.Params {
		t.Fatal("不同抽样数应产生不同指纹")
	}
	if s.Usable(other) {
		t.Error("参数变了缓存应整体失效")
	}
	if s.Usable("") {
		t.Error("空指纹不该被判定为可用")
	}
	if New("p").Usable("p") {
		t.Error("空缓存不该被判定为可用")
	}
}

// TestPutUsesAbsolutePath 换工作目录也要能命中,所以内部统一存绝对路径。
func TestPutUsesAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.mkv")
	os.WriteFile(file, []byte("x"), 0o644)

	s := New("p")
	s.Put("a.mkv", 1, time.Now(), Entry{Head: 30, OK: true}) // 故意传相对路径
	for p := range s.Files {
		if !filepath.IsAbs(p) {
			t.Errorf("缓存里的路径应为绝对路径, 实际 %q", p)
		}
	}
}

// TestSaveIsAtomic 写缓存不能留下 .tmp 残留。
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "cache.json")
	s := New("p")
	if err := s.Save(path); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("缓存文件应存在: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("不应留下临时文件: %s", e.Name())
		}
	}
}

// TestLoadCorrupt 缓存损坏时报错而不是静默当空处理 —— 调用方会提示用户。
func TestLoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	os.WriteFile(path, []byte("{不是 JSON"), 0o644)
	if _, err := Load(path); err == nil {
		t.Error("损坏的缓存应报错")
	}
}

// TestCleanNote 反复读写的说明不该越叠越长。
func TestCleanNote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"片头切点 30.0s", "片头切点 30.0s"},
		{"[一致 3/4] 片头切点 30.0s", "片头切点 30.0s"},
		{NotePrefix + "片头切点 30.0s", "片头切点 30.0s"},
		{"[一致 3/4] " + NotePrefix + "[一致 12/12] 片头切点 30.0s", "片头切点 30.0s"},
	}
	for _, c := range cases {
		if got := CleanNote(c.in); got != c.want {
			t.Errorf("CleanNote(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}
