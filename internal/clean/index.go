package clean

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/fsx"
)

// trimIndexes 裁掉所有指向「已经删掉的会话」的索引条目。
//
// 只处理 deleted 集合里的 ID：正文删除失败或路径越界被拒的会话，索引必须原样
// 保留。反过来做（先裁索引再看正文删没删）会制造幽灵条目 —— 用户点进去是空的，
// 而真正的文件其实还在磁盘上，属于最糟糕的中间态。
//
// 单个索引出错只记 ItemError：一个 Agent 的索引格式坏了，不该让另外 20 个
// Agent 的删除整批回滚。索引不存在或结构认不出时直接跳过，连错误都不记。
func (c *Cleaner) trimIndexes(
	spec catalog.Spec,
	home string,
	deleted map[string]bool,
	j *journal,
	res *Result,
	namer *backupNamer,
) {
	seenDB := map[string]bool{}
	for _, idx := range spec.Indexs {
		switch idx.Kind {
		case "jsonl-lines", "json-array", "json-dict":
			c.trimJSONIndex(idx, home, deleted, j, res, namer)
		case "sqlite-rows":
			// 配置不完整（如 VS Code 的 state.vscdb 只有库名、没有表名）说明这个
			// 索引在当前版本里还认不出来。跳过比猜测表名安全。
			if idx.DB == "" || idx.Table == "" || idx.IDCol == "" {
				continue
			}
			dbPath, err := resolveInHome(home, idx.DB)
			if err != nil {
				res.Errors = append(res.Errors, ItemError{
					Message: fmt.Sprintf("索引 %s 的数据库路径不安全: %v", idx.DB, err),
				})
				continue
			}
			// 同一 (库, 表) 只处理一次；同一库的不同表必须分别裁，因为每张表
			// 都可能有自己的会话引用。备份按表各做一份，Undo 逆序覆盖回去后
			// 仍然得到原始库。
			signature := dbPath + "\x00" + idx.Table + "\x00" + idx.IDCol
			if seenDB[signature] {
				continue
			}
			seenDB[signature] = true
			c.trimSQLiteIndex(idx, dbPath, deleted, j, res, namer)
		default:
			// 未收录的 Kind 属于未来版本或用户自定义格式，静默跳过。
		}
	}
}

// trimJSONIndex 处理一个 json 索引声明的全部匹配文件。
func (c *Cleaner) trimJSONIndex(
	idx catalog.Index,
	home string,
	deleted map[string]bool,
	j *journal,
	res *Result,
	namer *backupNamer,
) {
	if idx.Glob == "" {
		return
	}
	for _, path := range fsx.Glob(home, idx.Glob) {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			res.Errors = append(res.Errors, ItemError{
				Message: fmt.Sprintf("读取索引 %s 失败: %v", path, err),
			})
			continue
		}

		var (
			out        []byte
			removeFile bool
			changed    bool
			recognized bool
		)
		switch idx.Kind {
		case "jsonl-lines":
			out, changed, recognized = trimJSONL(idx, data, deleted)
		case "json-array":
			out, removeFile, changed, recognized = trimJSONArray(idx, data, deleted)
		case "json-dict":
			out, changed, recognized = trimJSONDict(idx, data, deleted)
		}
		if !recognized || !changed {
			continue
		}

		// 到这里才动刀：先整文件备份，备份失败就放弃这个索引。
		backup, err := c.backupFile(path, j.BatchID, namer)
		if err != nil {
			res.Errors = append(res.Errors, ItemError{
				Message: fmt.Sprintf("备份索引 %s 失败，已跳过该索引: %v", path, err),
			})
			continue
		}
		// 先记账、后动刀：备份条目落盘之后才删/改文件，崩溃窗口里账本已经
		// 知道这个索引被备份过。
		j.Indexes = append(j.Indexes, journalBackup{Orig: path, Backup: backup})
		if err := j.save(); err != nil {
			popLastJournalIndex(j)
			j.persistErr = err
			return
		}
		journalBeforeActionHook()

		if removeFile {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				popLastJournalIndex(j)
				if saveErr := j.save(); saveErr != nil {
					j.persistErr = saveErr
					return
				}
				res.Errors = append(res.Errors, ItemError{
					Message: fmt.Sprintf("删除已裁空的索引 %s 失败: %v", path, err),
				})
			}
			continue
		}
		if err := fsx.WriteFileAtomic(path, out, info.Mode().Perm()); err != nil {
			popLastJournalIndex(j)
			if saveErr := j.save(); saveErr != nil {
				j.persistErr = saveErr
				return
			}
			res.Errors = append(res.Errors, ItemError{
				Message: fmt.Sprintf("写回索引 %s 失败: %v", path, err),
			})
		}
	}
}

// trimJSONL 逐行过滤 JSONL。
//
// 解析失败的行原样保留，这是有意的保守：会话文件可能正在被写，末行经常是半截。
// 如果我们把「看不懂」当成「可删」，就会在一次并发写入时毁掉用户最后一条消息。
func trimJSONL(idx catalog.Index, data []byte, deleted map[string]bool) (out []byte, changed, recognized bool) {
	recognized = true
	if len(data) == 0 {
		return nil, false, true
	}
	text := string(data)
	trailingNewline := strings.HasSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if trailingNewline {
		lines = lines[:len(lines)-1]
	}

	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			kept = append(kept, line)
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
			kept = append(kept, line)
			continue
		}
		if objectMatches(obj, idx.Key, deleted) {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	if !changed {
		return nil, false, true
	}
	if len(kept) == 0 {
		return []byte{}, true, true
	}
	result := strings.Join(kept, "\n")
	if trailingNewline {
		result += "\n"
	}
	return []byte(result), true, true
}

// trimJSONArray 处理四种 json-array 形状：
//  1. 顶层数组 `[...]`
//  2. `{Path: [...]}` 包装数组
//  3. `{Path: {...}}` 包装字典（Claude 的 sessions-index.json 习惯）
//  4. 顶层字典 `{id: {...}}`
//
// 返回 removeFile 表示「裁空后整个文件可以消失」。这是契约要求的语义：留下一个
// 空数组/空字典，Agent 启动时可能把它当成损坏的索引而不是空索引。
func trimJSONArray(idx catalog.Index, data []byte, deleted map[string]bool) (out []byte, removeFile, changed, recognized bool) {
	root, err := decodeJSON(data)
	if err != nil {
		return nil, false, false, false
	}
	key := idx.Key
	switch doc := root.(type) {
	case []any:
		kept, dropped := filterArray(doc, key, deleted)
		if !dropped {
			return nil, false, false, true
		}
		if len(kept) == 0 {
			return nil, true, true, true
		}
		encoded, err := marshalJSON(kept)
		if err != nil {
			return nil, false, false, false
		}
		return encoded, false, true, true

	case map[string]any:
		// Path 为空不等于「一定是顶层字典」：内置的 Claude Code 索引就是
		// `{"version":1,"entries":[...]}`，Path 没写。这里对常见包装键做一次
		// 窄范围自动识别，否则这类文件会被当成顶层字典而一条都裁不掉。
		containerKey := idx.Path
		if containerKey == "" {
			containerKey = detectContainer(doc)
		}
		if containerKey != "" {
			container, ok := doc[containerKey]
			if !ok {
				return nil, false, false, false
			}
			switch value := container.(type) {
			case []any:
				kept, dropped := filterArray(value, key, deleted)
				if !dropped {
					return nil, false, false, true
				}
				if len(kept) == 0 {
					return nil, true, true, true
				}
				doc[containerKey] = kept
			case map[string]any:
				kept, dropped := filterDict(value, key, deleted)
				if !dropped {
					return nil, false, false, true
				}
				if len(kept) == 0 {
					return nil, true, true, true
				}
				doc[containerKey] = kept
			default:
				return nil, false, false, false
			}
			encoded, err := marshalJSON(doc)
			if err != nil {
				return nil, false, false, false
			}
			return encoded, false, true, true
		}

		kept, dropped := filterDict(doc, key, deleted)
		if !dropped {
			return nil, false, false, true
		}
		if len(kept) == 0 {
			return nil, true, true, true
		}
		encoded, err := marshalJSON(kept)
		if err != nil {
			return nil, false, false, false
		}
		return encoded, false, true, true

	default:
		return nil, false, false, false
	}
}

// trimJSONDict 只按字典键删。
//
// json-dict 的语义是「键就是 ID」，不像 json-array 那样还允许对象里带 Key 字段。
// 这里保持严格，避免把 `{"sessions": {...}}` 这种包装层的键当成会话。
func trimJSONDict(idx catalog.Index, data []byte, deleted map[string]bool) (out []byte, changed, recognized bool) {
	root, err := decodeJSON(data)
	if err != nil {
		return nil, false, false
	}
	doc, ok := root.(map[string]any)
	if !ok {
		return nil, false, false
	}
	if idx.Path != "" {
		child, ok := doc[idx.Path].(map[string]any)
		if !ok {
			return nil, false, false
		}
		doc = child
	}
	kept := make(map[string]any, len(doc))
	for key, value := range doc {
		if deleted[key] {
			changed = true
			continue
		}
		kept[key] = value
	}
	if !changed {
		return nil, false, true
	}
	// json-dict 不删文件：一个空字典在多数 Agent 里是合法状态，删文件反而可能
	// 让它在下次启动时重建出别的默认内容。
	encoded, err := marshalJSON(kept)
	if err != nil {
		return nil, false, false
	}
	if idx.Path != "" {
		outer := root.(map[string]any)
		outer[idx.Path] = kept
		encoded, err = marshalJSON(outer)
		if err != nil {
			return nil, false, false
		}
	}
	return encoded, true, true
}

// detectContainer 在 Index.Path 缺省时，从顶层对象里找一个常见的包装键。
//
// 只认这些固定名字，不做「随便找一个数组」的泛化猜测：后者会把带元数据的
// 顶层字典误判成包装层。代价是自定义格式仍需显式声明 Path，这是可接受的 ——
// 认不出来时跳过，比乱猜结构安全。
func detectContainer(doc map[string]any) string {
	for _, key := range []string{
		"sessions", "entries", "items", "data", "records", "threads", "conversations",
	} {
		value, ok := doc[key]
		if !ok {
			continue
		}
		switch value.(type) {
		case []any, map[string]any:
			return key
		}
	}
	return ""
}

// filterArray 返回保留下来的元素，以及是否发生过删除。
func filterArray(items []any, key string, deleted map[string]bool) ([]any, bool) {
	kept := make([]any, 0, len(items))
	dropped := false
	for _, item := range items {
		if elementMatches(item, key, deleted) {
			dropped = true
			continue
		}
		kept = append(kept, item)
	}
	return kept, dropped
}

// filterDict 处理「键是 ID」和「值里带 Key 字段」两种字典形态。
//
// 两种都认，是因为同一份 sessions-index.json 在不同版本里两种写法都出现过：
// 一种把 sessionId 当键，另一种把条目塞在 entries 里、sessionId 放值里。
func filterDict(doc map[string]any, key string, deleted map[string]bool) (map[string]any, bool) {
	kept := make(map[string]any, len(doc))
	dropped := false
	for k, value := range doc {
		if deleted[k] {
			dropped = true
			continue
		}
		if child, ok := value.(map[string]any); ok && objectMatches(child, key, deleted) {
			dropped = true
			continue
		}
		kept[k] = value
	}
	return kept, dropped
}

func elementMatches(item any, key string, deleted map[string]bool) bool {
	switch value := item.(type) {
	case string:
		return deleted[value]
	case map[string]any:
		return objectMatches(value, key, deleted)
	}
	return false
}

// objectMatches 按 Index.Key 找会话 ID。
//
// Key 为空时退到几个业界通用字段。自定义适配器忘了写 key 是常见的手滑，
// 与其整个索引不裁，不如按最可能的字段尽力一次 —— 误删风险由「必须精确等于
// 待删 ID」兜底，不会因为字段猜错而多删。
func objectMatches(obj map[string]any, key string, deleted map[string]bool) bool {
	if key != "" {
		return idValueMatches(obj[key], deleted)
	}
	for _, candidate := range []string{"id", "sessionId", "session_id", "uid", "uuid"} {
		if value, ok := obj[candidate]; ok && idValueMatches(value, deleted) {
			return true
		}
	}
	return false
}

// idValueMatches 处理 JSON 里 ID 的各种落盘类型。
//
// 用 json.Decoder.UseNumber 解码，数字会保持 json.Number；如果这里只认 string，
// 数字型主键的索引就会永远匹配不上，删除后留下幽灵条目。
func idValueMatches(value any, deleted map[string]bool) bool {
	switch v := value.(type) {
	case string:
		return deleted[v]
	case json.Number:
		return deleted[v.String()]
	case float64:
		return deleted[strconv.FormatFloat(v, 'f', -1, 64)]
	case int64:
		return deleted[strconv.FormatInt(v, 10)]
	case int:
		return deleted[strconv.Itoa(v)]
	}
	return false
}

// decodeJSON 带 UseNumber 的解码，避免大整数 ID 被 float64 截断。
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

// marshalJSON 固定 2 空格缩进。encoding/json 对 map 键自动排序，
// 因此同一份索引裁两次得到的字节是一致的，方便用户用 git/diff 对比。
func marshalJSON(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// sortedIDs 返回排序后的 ID 列表，给 SQL 的 IN 参数与 journal 用。
// 稳定的顺序让 journal 可读、让测试可断言，也避免 IN 子句参数顺序随机变化。
func sortedIDs(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
