// Package dcache 持久化 -auto 的识别结论。
//
// 动机:静音检测要整片解码音轨,是 -auto 唯一的耗时大头。而一部剧的
// OP/ED 时长全剧一致,同一批文件反复识别纯属浪费 —— 尤其是容器
// (docker run --rm)每次都是全新进程,不落盘就永远记不住。
//
// 缓存以「绝对路径 → 切点」的形式记录,并用文件大小 + 修改时间校验:
// 文件被替换或重新压制过就当作没见过,重新识别。
package dcache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// SchemaVersion 是缓存文件的结构版本。结构不兼容时整体失效,直接重新识别。
const SchemaVersion = 1

// Entry 是单个文件的识别结论。
type Entry struct {
	Size int64 `json:"size"`
	// MTime 是文件修改时间(Unix 秒),与 Size 一起用于判断文件是否变过。
	MTime  int64   `json:"mtime"`
	Head   float64 `json:"head"`
	Tail   float64 `json:"tail"`
	OK     bool    `json:"ok"`
	Note   string  `json:"note"`
	Show   string  `json:"show"`
	Season int     `json:"season"`
	// At 是本条记录的写入时间,仅用于排查。
	At time.Time `json:"at"`
}

// Store 是一份缓存文件的内存表示。
//
// Params 记录识别参数(算法版本、阈值、抽样数等);参数变了整份缓存失效,
// 避免把旧算法算出的切点当成新算法的结论用。
type Store struct {
	Version int              `json:"version"`
	Params  string           `json:"params"`
	Files   map[string]Entry `json:"files"`
}

// Params 描述一次识别的参数指纹。任一项变化都会让旧的缓存整体失效。
type Params struct {
	NoiseDB    float64
	MinSilence float64
	Sample     int
}

// Signature 给出参数指纹,同时把结构版本编进去。
func (p Params) Signature() string {
	return fmt.Sprintf("schema=%d noise=%g min=%g sample=%d",
		SchemaVersion, p.NoiseDB, p.MinSilence, p.Sample)
}

// New 建立一份空缓存。
func New(params string) *Store {
	return &Store{Version: SchemaVersion, Params: params, Files: map[string]Entry{}}
}

// Load 读取缓存文件。文件不存在时返回空缓存而非错误 —— 首次运行本来就没缓存。
// 文件存在但损坏时返回错误,由调用方决定是否忽略。
func Load(path string) (*Store, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return New(""), nil
		}
		return nil, err
	}
	var s Store
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("缓存文件 %s 解析失败: %w", path, err)
	}
	if s.Files == nil {
		s.Files = map[string]Entry{}
	}
	return &s, nil
}

// Save 把缓存写回文件。先写临时文件再 rename,避免中途失败留下半截 JSON。
func (s *Store) Save(path string) error {
	if s == nil {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建缓存目录 %s 失败: %w", dir, err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Usable 判断这份缓存能否用于指定参数的识别。
func (s *Store) Usable(params string) bool {
	return s != nil && s.Version == SchemaVersion && s.Params == params && len(s.Files) > 0
}

// Lookup 取出某文件的缓存结论。
//
// 命中条件:缓存里存在、标记为成功、且文件大小与修改时间与记录一致。
// size<=0 表示调用方没提供大小,此时只比对修改时间。
func (s *Store) Lookup(path string, size int64) (Entry, bool) {
	if s == nil {
		return Entry{}, false
	}
	e, ok := s.Files[path]
	if !ok || !e.OK {
		return Entry{}, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return Entry{}, false
	}
	if size > 0 && e.Size > 0 && e.Size != info.Size() {
		return Entry{}, false
	}
	if e.MTime > 0 && e.MTime != info.ModTime().Unix() {
		return Entry{}, false
	}
	return e, true
}

// Put 写入(或覆盖)一条结论。path 会被转成绝对路径,保证换个工作目录也能命中。
func (s *Store) Put(path string, size int64, mtime time.Time, e Entry) {
	if s == nil {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if s.Files == nil {
		s.Files = map[string]Entry{}
	}
	e.Size = size
	e.MTime = mtime.Unix()
	e.At = time.Now()
	s.Files[abs] = e
}

// CleanNote 去掉不适合长期保存的说明前缀(一致率标记、缓存来源标记),
// 免得每次读出来再写回去时前缀越叠越长。
//
// 前缀可能叠了好几层(一致率标记 + 缓存标记再来一轮),所以循环剥离到不再变化。
func CleanNote(note string) string {
	for i := 0; i < 4; i++ {
		trimmed := strings.TrimSpace(agreePrefixRe.ReplaceAllString(note, ""))
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, NotePrefix))
		if trimmed == note {
			break
		}
		note = trimmed
	}
	return strings.TrimSpace(note)
}

// NotePrefix 是"结论来自缓存"时加在说明前面的标记。
const NotePrefix = "沿用上次识别 · "

// agreePrefixRe 匹配说明开头的一致率标记,如 "[一致 3/4] "。
var agreePrefixRe = regexp.MustCompile(`^\[一致 \d+/\d+\]\s*`)
