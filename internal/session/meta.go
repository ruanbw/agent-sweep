package session

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Meta 是从会话正文里尽力抽出来的元数据。
//
// 全部字段都可能为零值：各家格式差异太大，抽不到就退回文件名 / mtime。
// 这里的原则是「宁可信息少，不可信息错」—— 标题抽错了用户会以为是会话串了。
type Meta struct {
	Title    string
	Project  string
	Time     time.Time
	Messages int
	Snippet  string
}

// 标题字段的候选键，按可信度排序。
var titleKeys = []string{
	"title", "name", "summary", "slug", "display", "firstMessage",
	"first_message", "prompt", "description", "conversation_title",
}

// 项目路径的候选键。
var projectKeys = []string{
	"cwd", "projectPath", "project_path", "project", "directory",
	"workspace", "workspacePath", "folder_paths", "folderPaths", "path",
}

// 时间字段的候选键。
var timeKeys = []string{
	"timestamp", "createdAt", "created_at", "updatedAt", "updated_at",
	"lastModified", "last_modified", "last_modified_time", "time_updated",
	"timeUpdated", "mtime", "date", "created", "updated",
}

// 这些角色的话绝不能当标题：它们是提示词 / 工具说明，不是用户说的话。
var nonUserRoles = map[string]bool{
	"system": true, "developer": true, "tool": true, "function": true,
}

// mergeNodeBudget 是一次 merge 内所有 JSON 遍历共享的节点预算。
//
// 一次 merge 最多会做 4 次树遍历（标题、用户正文、深层标题、content 数组），
// 而 deepUserText / deepTitle 只靠深度上限（8 / 6）兜底：每层最多展开 9 / 7
// 个候选键，最坏情况是 9^8 ≈ 4300 万、7^6 ≈ 12 万次访问。会话头部又允许
// 1MB 一行，扫描器同时跑 16 个文件 worker，一条畸形但合法的深宽记录就足以
// 把 CPU 打到尖峰、让整个扫描长时间无响应。
//
// 4096 的依据：真实会话（Claude Code / Pi / DSH 等）一条记录通常只有几十个
// 节点，即使整份会话是一个大 JSON 对象、含几百条消息，也远低于这个量级；
// 同时它又比 1MB 单行常见的几万到几十万节点低一到两个数量级。预算按 merge
// 共享（而不是每个函数各给一份），保证单条记录 4 次遍历的总访问量有上界。
const mergeNodeBudget = 4096

// budget 是遍历预算。left 是剩余节点数，used 只用于测试断言。
// 预算耗尽不是错误：遍历就地停止，已收集到的内容照常返回，
// 「解析失败退回 fallback」的语义不变。
type budget struct {
	left int
	used int
}

func newBudget() *budget {
	return &budget{left: mergeNodeBudget}
}

// spend 消耗一个节点并报告是否还能继续向下遍历。
// nil 预算表示不限制，保留给直接调用内部函数的场景。
func (b *budget) spend() bool {
	if b == nil {
		return true
	}
	if b.left <= 0 {
		return false
	}
	b.left--
	b.used++
	return true
}

// exhausted 用于在宽数组 / 宽 map 的循环里尽早退出：
// 预算耗尽后递归调用会立刻返回，但外层仍会把几万个元素挨个走完。
func (b *budget) exhausted() bool {
	return b != nil && b.left <= 0
}

// ReadMeta 读一个会话文件并抽元数据。
//
// 关键设计：**按需读，而不是固定读 64KB**。
// Pi 的会话前 71KB 是系统提示词，固定 64KB 会一条标题都抽不到，
// 列表里全变成「会话 2026-10-」。这里从 64KB 起、每次翻 4 倍、
// 最多 1MB，**一旦拿到标题就停** —— 绝大多数文件第一次就结束，
// 只有 Pi 这种系统提示词特别长的才多读一轮。
func ReadMeta(path string, jsonl bool) Meta {
	const (
		first = 64 * 1024
		last  = 1024 * 1024
	)
	var meta Meta
	file, err := os.Open(path)
	if err != nil {
		return meta
	}
	defer file.Close()

	// 缓冲区跨轮复用：扩容只增长，不再为已经读过的内容重新分配。
	// 每轮仍从头读 —— 上一轮读到的只是前缀，重新读一遍比记账简单。
	var buf []byte
	for limit := int64(first); limit <= last; limit *= 4 {
		if int64(cap(buf)) < limit {
			buf = make([]byte, limit)
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return meta
		}
		head, err := readHeadInto(file, buf[:limit])
		if err != nil {
			return meta
		}
		if jsonl {
			meta = ParseJSONL(head)
		} else {
			meta = ParseJSONObject(head)
		}
		// readHeadInto 保证：err == nil 且 len(head) < limit 只可能是真的读完了。
		// 短读已经被它内部读满，不会走到这里被误判成 EOF。
		if meta.Title != "" || int64(len(head)) < limit {
			return meta
		}
	}
	return meta
}

// ReadMetaZstd 从 zstd 压缩的 JSONL 里抽元数据（DeepSeek Harness 就是这么存的）。
//
// 解压是有上限的：只解前 1MB 压缩流、最多 512KB 明文。
// 会话正文可能有几百 MB，为了一个标题把整份解压出来是不可接受的 ——
// 而标题、cwd、时间戳都在头几行。
func ReadMetaZstd(path string) Meta {
	file, err := os.Open(path)
	if err != nil {
		return Meta{}
	}
	defer file.Close()

	decoder, err := zstd.NewReader(io.LimitReader(file, 1<<20))
	if err != nil {
		return Meta{}
	}
	defer decoder.Close()

	head, _ := io.ReadAll(io.LimitReader(decoder, 512*1024))
	return ParseJSONL(head)
}

// ReadMetaDir 在一个「目录即会话」的目录里找可读元数据：
// 先认常见的明文文件名，再认 zstd 压缩的 JSONL。都找不到就返回空 Meta。
//
// 这一层没有时再下探一层子目录：有些 Agent 把正文放在 <会话目录>/run-0/ 下
// （Pi 的重跑布局就是这样，run-0 是第一次跑，run-1 是重试）。只下探一层，
// 且按名字排序取第一个 —— 深递归会在会话目录很大时变成新的耗时点。
func ReadMetaDir(dir string) Meta {
	if meta := readMetaDirNames(dir); meta.Title != "" || meta.Project != "" {
		return meta
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Meta{}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		// 隐藏项不进；只认目录，因为要下探的是 run-N 这种子目录。
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		if meta := readMetaDirNames(filepath.Join(dir, name)); meta.Title != "" || meta.Project != "" {
			return meta
		}
	}
	return Meta{}
}

// readMetaDirNames 只在 dir 这一层按名字找元数据，不再下探。
func readMetaDirNames(dir string) Meta {
	names := []string{
		"api_conversation_history.json", "conversation.json", "session.json",
		// 明文 JSONL：Pi 的正文就是 session.jsonl（放在 run-N/ 下）。
		// 以前只有 .json 与两个 .zstd 变体，导致这类会话标题一律退化成 uuid。
		"session.jsonl",
		// Grok 的目录型会话把标题放在 <workspace>/<id>/summary.json，正文是
		// 同目录的 updates.jsonl（ACP 事件流，不含标题）。不认这个文件名，
		// 标题只能退化成「会话 <uuid8>」。
		"meta.json", "summary.json", "messages.json",
		// 压缩变体固定放最后：解压比读明文贵，能明文拿到就不碰压缩包。
		"session.v4.jsonl.zstd", "session.jsonl.zstd",
	}
	for _, name := range names {
		file := filepath.Join(dir, name)
		if strings.HasSuffix(name, ".zstd") {
			if meta := ReadMetaZstd(file); meta.Title != "" || meta.Project != "" {
				return meta
			}
			continue
		}
		if meta := ReadMeta(file, strings.HasSuffix(name, ".jsonl")); meta.Title != "" || meta.Project != "" {
			return meta
		}
	}
	return Meta{}
}

func readHead(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return readHeadFrom(file, limit)
}

// readHeadFrom 从 r 读前 limit 字节，抽出 io.Reader 是为了让测试能注入
// 短读 reader；readHead 只是「打开文件」的薄包装。
func readHeadFrom(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, nil
	}
	return readHeadInto(r, make([]byte, limit))
}

// readHeadInto 读满 buf，或者读到真正的 io.EOF 为止。
//
// 这里必须用 io.ReadFull，不能只调一次 Read：Read 允许短读（管道、网络卷、
// 加密卷、某些 FUSE 都会一次只给一部分字节），短读并不代表文件结束。
// 单次 Read 返回 n < len(buf) 时旧代码把它当成 EOF，直接放弃标题，用户
// 看到的就是「未命名会话」。io.ReadFull 会在内部把短读循环补满，只有底层
// 真的给出 io.EOF 时才提前返回（n > 0 时转成 io.ErrUnexpectedEOF）。
//
// 因此返回值对调用方的保证是：
//   - err == nil 且 len(out) == len(buf)：读满；
//   - err == nil 且 len(out) < len(buf)：文件确实读完了，短读已被消化；
//   - err != nil：真的 I/O 错误（空文件、短文件都不会走到这里）。
//
// 把 buf 作为参数而不是内部 make，是为了让 ReadMeta 扩容时复用同一块缓冲。
func readHeadInto(r io.Reader, buf []byte) ([]byte, error) {
	if len(buf) == 0 {
		return nil, nil
	}
	n, err := io.ReadFull(r, buf)
	switch err {
	case nil, io.ErrUnexpectedEOF:
		// 读满；或文件比 buf 短，UnexpectedEOF 在这里就是正常读完。
		return buf[:n], nil
	case io.EOF:
		// 一个字节都没有：空文件不是错误（调用方按 0 字节处理）。
		return buf[:0], nil
	default:
		// 真实 I/O 错误。已经读到的字节仍然可用（保持旧行为），
		// 只有一个字节都没读到时才算失败。
		if n == 0 {
			return nil, err
		}
		return buf[:n], nil
	}
}

// ParseJSONL 从 JSONL 头部抽元数据。
//
// 逐行解析而不是整体 unmarshal：会话文件经常正在被写，末行是半截，
// 整体解析会直接失败，逐行只有坏的那行被跳过。
func ParseJSONL(head []byte) Meta {
	var meta Meta
	for _, line := range bytes.Split(head, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(line, &obj); err != nil {
			continue
		}
		meta.merge(obj)
		// 拿到标题且已经数过几条消息就可以停了：再往后翻收益递减。
		if meta.Title != "" && meta.Messages > 2 {
			break
		}
	}
	return meta
}

// ParseJSONObject 从单条 JSON（可能是被截断的）里抽元数据。
func ParseJSONObject(head []byte) Meta {
	var meta Meta
	var obj map[string]any
	if err := json.Unmarshal(head, &obj); err == nil {
		meta.merge(obj)
		return meta
	}
	// 截断的 JSON：用逐行兜底，把能认出来的字符串捞出来。
	if strings.Contains(string(head), "\"messages\"") || strings.Contains(string(head), "\"conversation\"") {
		meta.mergeLoose(head)
	}
	return meta
}

// merge 把一条记录里的字段并进结果。后到的只填空，不覆盖已有值。
func (m *Meta) merge(obj map[string]any) {
	// 一次 merge 的所有遍历共用这一份预算：4 次遍历各自从头上限的话，
	// 单条记录的总开销仍会翻 4 倍，挡不住深宽 JSON。
	b := newBudget()
	role := roleOf(obj)

	// 标题：先认显式标题字段，再退回消息正文。
	if m.Title == "" {
		for _, key := range titleKeys {
			if v, ok := obj[key].(string); ok && strings.TrimSpace(v) != "" {
				m.Title = clean(v)
				break
			}
		}
	}
	if m.Title == "" && !nonUserRoles[role] {
		if text := contentText(contentOf(obj), b); text != "" && !isBoilerplate(text) {
			m.Title = clean(text)
		}
	}
	// 有些 Agent 把消息裹了好几层（DSH 是 data.inserted[].content[]）。
	// 深搜一次找「第一条 role=user 的正文」，比给每家写一个结构解析器划算。
	if m.Title == "" {
		if text := deepUserText(obj, 0, b); text != "" {
			m.Title = clean(text)
		}
	}
	// 还有一种：标题不在顶层，而在 payload.meta.title 这种包装层里
	// （billion-context 就是）。只在还没标题时深搜，避免把内层工具消息当标题。
	if m.Title == "" {
		if text := deepTitle(obj, 0, b); text != "" {
			m.Title = clean(text)
		}
	}

	if m.Project == "" {
		for _, key := range projectKeys {
			switch v := obj[key].(type) {
			case string:
				if v != "" && strings.HasPrefix(v, "/") {
					m.Project = v
				}
			case []any:
				if len(v) > 0 {
					if s, ok := v[0].(string); ok && strings.HasPrefix(s, "/") {
						m.Project = s
					}
				}
			}
			if m.Project != "" {
				break
			}
		}
	}
	if m.Time.IsZero() {
		for _, key := range timeKeys {
			if t, ok := parseTime(obj[key]); ok {
				m.Time = t
				break
			}
		}
	}
	// 时间也可能藏在 message 里（Pi 的 message.timestamp 是毫秒）。
	if m.Time.IsZero() {
		if msg, ok := obj["message"].(map[string]any); ok {
			for _, key := range timeKeys {
				if t, ok := parseTime(msg[key]); ok {
					m.Time = t
					break
				}
			}
		}
	}

	// 消息计数：role 和 type 是同一件事的两种标法，**一条记录最多计一条**。
	// Claude Code 的形态是 {"type":"user","message":{"role":"user",…}}，
	// 两个字段同时命中；早先两个 if 各自 ++ 会把消息数翻倍。
	// 也不能只留 type：有的 Agent（Pi / Gemini CLI 一类）只有 type 没有 role，
	// 反过来也有只写 role 的。
	typ, _ := obj["type"].(string)
	if isChatMessage(role) || isChatMessage(typ) {
		m.Messages++
	}

	if m.Snippet == "" && !nonUserRoles[role] {
		if text := contentText(contentOf(obj), b); text != "" && !isBoilerplate(text) {
			m.Snippet = clean(text)
		}
	}
}

// deepTitle 在嵌套结构里找显式标题字段（title / name / summary / slug）。
//
// 与 deepUserText 分开：标题是「作者写的」，正文是「用户说的」，
// 前者可信度高，所以先找标题；但两者都可能藏在包装层里。
func deepTitle(value any, depth int, b *budget) string {
	if depth > 6 || !b.spend() {
		return ""
	}
	switch v := value.(type) {
	case map[string]any:
		for _, key := range titleKeys {
			if text, ok := v[key].(string); ok {
				text = strings.TrimSpace(text)
				if text != "" {
					return text
				}
			}
		}
		for _, key := range []string{"payload", "meta", "data", "conversation", "session", "thread", "details"} {
			if b.exhausted() {
				return ""
			}
			if child, ok := v[key]; ok {
				if text := deepTitle(child, depth+1, b); text != "" {
					return text
				}
			}
		}
	case []any:
		for _, item := range v {
			if b.exhausted() {
				return ""
			}
			if text := deepTitle(item, depth+1, b); text != "" {
				return text
			}
		}
	}
	return ""
}

// deepUserText 在任意嵌套结构里找第一条 role=user 的正文。
// 只认 user：system / tool 动辄几万字，当标题等于把提示词贴进列表。
func deepUserText(value any, depth int, b *budget) string {
	if depth > 8 || !b.spend() {
		return ""
	}
	switch v := value.(type) {
	case map[string]any:
		if role, _ := v["role"].(string); role == "user" {
			for _, key := range []string{"content", "parts", "text", "message"} {
				// 环境说明（<system-reminder> 等）也是 role=user，但不该当标题；
				// 跳过它继续往下找，找不到就退回文件名。
				if text := contentText(v[key], b); text != "" && !isBoilerplate(text) {
					return text
				}
			}
		}
		for _, key := range []string{"content", "parts", "message", "data", "inserted", "messages", "items", "payload", "event"} {
			if b.exhausted() {
				return ""
			}
			if child, ok := v[key]; ok {
				if text := deepUserText(child, depth+1, b); text != "" {
					return text
				}
			}
		}
	case []any:
		for _, item := range v {
			if b.exhausted() {
				return ""
			}
			if text := deepUserText(item, depth+1, b); text != "" {
				return text
			}
		}
	}
	return ""
}

// isChatMessage 判断一个 role / type 取值是否代表一条对话消息。
//
// 只认 user / assistant：system、tool、summary、message、session 等
// 是提示词、工具输出或容器/元信息，计进消息数会让「这条会话有多大」
// 虚高，而用户是拿这个数字判断删不删的。
func isChatMessage(v string) bool {
	return v == "user" || v == "assistant"
}

func roleOf(obj map[string]any) string {
	if role, ok := obj["role"].(string); ok {
		return role
	}
	if msg, ok := obj["message"].(map[string]any); ok {
		if role, ok := msg["role"].(string); ok {
			return role
		}
	}
	return ""
}

func contentOf(obj map[string]any) any {
	if content, ok := obj["content"]; ok {
		return content
	}
	if msg, ok := obj["message"].(map[string]any); ok {
		if content, ok := msg["content"]; ok {
			return content
		}
	}
	return nil
}

// mergeLoose 是截断 JSON 的兜底：只认 "key":"value" 形状。
func (m *Meta) mergeLoose(head []byte) {
	s := string(head)
	for _, key := range titleKeys {
		if m.Title != "" {
			break
		}
		if v, ok := looseString(s, key); ok {
			m.Title = clean(v)
		}
	}
}

// contentText 把各种 content 形态压成纯文本：
// 字符串、[{type:"text", text:"…"}]、{text:"…"}。
// 数组可以非常宽（一条记录里塞几万个 content 块），所以逐项消耗预算；
// 预算耗尽就带着已收集到的部分文本返回，不报错。
func contentText(value any, b *budget) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		var parts []string
		for _, item := range v {
			if !b.spend() {
				break
			}
			if s, ok := item.(string); ok {
				parts = append(parts, s)
				continue
			}
			if obj, ok := item.(map[string]any); ok {
				if text, ok := obj["text"].(string); ok && text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	case map[string]any:
		if !b.spend() {
			return ""
		}
		if text, ok := v["text"].(string); ok {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

// parseTime 认三种真实存在的形态：epoch（秒/毫秒/纳秒）、ISO 字符串、Unix 数字字符串。
func parseTime(value any) (time.Time, bool) {
	switch v := value.(type) {
	case float64:
		return fromEpoch(int64(v)), true
	case int64:
		return fromEpoch(v), true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return fromEpoch(n), true
		}
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return time.Time{}, false
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return fromEpoch(n), true
		}
		for _, layout := range []string{
			time.RFC3339Nano, time.RFC3339,
			"2006-01-02T15:04:05.000Z",
			"2006-01-02 15:04:05",
			"2006-01-02T15:04:05",
		} {
			if t, err := time.Parse(layout, s); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// fromEpoch 按量级判断单位。1e12 以上是毫秒，1e17 以上是纳秒 ——
// 各家混用得很随意，按阈值猜比要求每款 Agent 各写一个解析器现实。
func fromEpoch(n int64) time.Time {
	switch {
	case n <= 0:
		return time.Time{}
	case n > 1e17:
		return time.Unix(0, n)
	case n > 1e12:
		return time.UnixMilli(n)
	default:
		return time.Unix(n, 0)
	}
}

func looseString(s, key string) (string, bool) {
	needle := "\"" + key + "\":"
	idx := strings.Index(s, needle)
	if idx < 0 {
		return "", false
	}
	rest := strings.TrimLeft(s[idx+len(needle):], " \t")
	if len(rest) < 2 || rest[0] != '"' {
		return "", false
	}
	rest = rest[1:]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// clean 压掉换行与多余空白，标题在列表里是单行渲染的。
// boilerplatePrefixes 是「不是用户说的话」的正文前缀。
//
// 有些 Agent 会往用户消息里塞一大段环境说明：WorkBuddy/CodeBuddy 是
// <system-reminder data-role="user-context"> 包住的 OS/Shell/Workspace 信息，
// 其他家也有 <user_info>、<env> 这类包装。它们 role=user、也确实是正文的一部分，
// 但拿来当标题毫无信息量（而且会占满 80 字，把真正的第一句挤掉）。
var boilerplatePrefixes = []string{
	"<system-reminder",
	"<user_info",
	"<environment",
	"<env",
	"<context",
	"# AGENTS.md",
	"Users/",
}

// isBoilerplate 判断一段用户正文是不是「环境说明」而不是真实发言。
// 只看前缀，且要求正文以这些标记开头 —— 用户自己写的 `<` 开头内容很少见，
// 即使误判，代价也只是退回到下一条消息，不会丢会话。
func isBoilerplate(s string) bool {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return true
	}
	for _, prefix := range boilerplatePrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func clean(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) > 80 {
		return strings.TrimSpace(string(runes[:80])) + "…"
	}
	return strings.TrimSpace(s)
}
