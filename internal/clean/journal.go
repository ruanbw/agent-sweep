package clean

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ruanbw/agent-sweep/internal/fsx"
)

// journal 是一次删除的完整账本，也是撤销的唯一依据。
//
// 分成 Moved / Indexes / SQLite 三块而不是一个通用条目列表，是因为撤销时
// 三者的动作不同：备份文件要覆盖回去，废纸篓条目要挪回来，SQLite 库虽然也是
// 覆盖，但覆盖前必须清掉 sidecar。类型分清楚，Undo 里就不需要 switch 猜。
type journal struct {
	BatchID string    `json:"batchId"`
	At      time.Time `json:"at"`
	Mode    string    `json:"mode"`
	// Home 是这次删除时的数据根，撤销时用它重放 checkPathInHome 护栏。
	//
	// journal 只是磁盘上一份可被改写（或由旧版本写入）的 JSON，里面的 Orig
	// 不能当成可信路径直接交给 movePath；没有这份数据根，撤销就无法判断
	// 「这个 Orig 当初是否在允许范围内」。omitempty 让旧 journal 读回来是
	// 空串，Undo 据此明确区分「旧格式」而不是把空串当成合法数据根。
	Home    string          `json:"home,omitempty"`
	Count   int             `json:"count"`
	Freed   int64           `json:"freed"`
	Agents  []string        `json:"agents"`
	Undone  bool            `json:"undone"`
	Moved   []journalMoved  `json:"moved"`
	Indexes []journalBackup `json:"indexes"`
	SQLite  []journalDB     `json:"sqlite"`

	// persist 是 Delete 在删除过程中挂上的落盘回调；nil 表示这是一份只读账本
	// （readJournal 读回来的、Undo 手里的、测试里手写的）。
	//
	// 放在这里而不是作为参数逐层传递，是因为 deleteSidecars 由 sqlite.go 调用，
	// 签名不能改；它只有 journal 可以拿到落盘能力。unexported 字段 encoding/json
	// 会直接忽略，不会改变 journal 的磁盘格式。
	persist func() error

	// persistErr 记录删除过程中第一次「journal 写不进去」。deleteSidecars 无法
	// 返回 error，只能记在这里，由 Delete 在阶段结束后统一检查并停手。
	persistErr error
}

// save 在挂有落盘回调时原子重写 journal；从磁盘读回来的账本没有回调，是 no-op。
func (j *journal) save() error {
	if j.persist == nil {
		return nil
	}
	return j.persist()
}

type journalMoved struct {
	Orig    string `json:"orig"`
	Trashed string `json:"trashed"`
}

type journalBackup struct {
	Orig   string `json:"orig"`
	Backup string `json:"backup"`
}

type journalDB struct {
	DB      string       `json:"db"`
	Backup  string       `json:"backup"`
	Deletes []journalRow `json:"deletes"`
}

// journalRow 记录「哪张表删了哪些 ID」。
//
// 记 ID 而不是行数，是为了让撤销/审计能回答「具体丢了哪条会话」；行数只能告诉
// 用户「少了 3 条」，那正是最让人不安的答案。
type journalRow struct {
	Table string   `json:"table"`
	IDs   []string `json:"ids"`
}

func (c *Cleaner) journalPath(batchID string) string {
	return filepath.Join(c.opts.JournalDir, batchID+".json")
}

func (c *Cleaner) writeJournal(j *journal) error {
	if err := os.MkdirAll(c.opts.JournalDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// journal 含会话 ID 与文件路径，属于用户隐私；0600 比「目录 0700」再收紧一层。
	return fsx.WriteFileAtomic(c.journalPath(j.BatchID), data, 0o600)
}

func (c *Cleaner) readJournal(batchID string) (*journal, error) {
	if err := validateBatchID(batchID); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(c.journalPath(batchID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("clean: 找不到批次 %s 的 journal", batchID)
		}
		return nil, err
	}
	var j journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("clean: 批次 %s 的 journal 已损坏: %w", batchID, err)
	}
	if j.BatchID == "" {
		j.BatchID = batchID
	}
	return &j, nil
}

// validateBatchID 防止 Undo 被传入 `../../something` 之类的值。
//
// batchID 最终会拼进 journal 路径；虽然调用方是内部 HTTP 层，但 API 参数就是
// 用户输入。这里只接受「单段文件名」，路径穿越在到达文件系统之前就被拒绝。
func validateBatchID(batchID string) error {
	if batchID == "" {
		return errors.New("clean: batchID 为空")
	}
	if batchID == "." || batchID == ".." {
		return fmt.Errorf("clean: 非法 batchID %q", batchID)
	}
	if strings.ContainsAny(batchID, `/\`) || filepath.Base(batchID) != batchID {
		return fmt.Errorf("clean: 非法 batchID %q", batchID)
	}
	return nil
}

// Undo 按 journal 逆序撤销一个批次。
//
// 顺序是「先还原所有备份，再把废纸篓挪回来」。先备份文件是因为索引/数据库
// 覆盖回去之后，正文恢复失败也不会让 Agent 面对一个指向不存在会话的索引；
// 反过来则会在中途留下「正文回来了、索引还是被裁过的」状态。
//
// 已经撤销过的批次直接返回 0，不报错：用户重复点撤销按钮是常态，
// 第二次点应该像第一次一样安静地成功。
func (c *Cleaner) Undo(batchID string) (UndoResult, error) {
	opMu.Lock()
	defer opMu.Unlock()

	j, err := c.readJournal(batchID)
	if err != nil {
		return UndoResult{Errors: []string{}}, err
	}
	// 撤销的可行性取决于「正文还能不能回来」，而不是 mode 的字面值：
	//
	//   - trash 批次：正文在废纸篓里，Moved 记录齐全，整批可逆。
	//   - permanent 但 journal 里有 SQLite 备份：这类会话的正文就是库里的行，
	//     journal 存了删行前的整库副本，整份覆盖回去即可完整恢复（正文与索引
	//     一起回来），所以照旧允许。
	//   - permanent 且没有任何 SQLite 备份：正文（jsonl / json / 目录）已经被
	//     os.RemoveAll 永久删除，journal 里没有正文副本，能做的只是把索引备份
	//     覆盖回去 —— 结果是「点进去是空」的幽灵会话，比不撤销更糟。拒绝。
	//   - 未知 mode（包括旧 journal 里的空值）按最后一条处理：宁可给出明确
	//     错误，也不要悄悄做一半。
	//
	// 校验放在任何磁盘操作和 j.Undone 标记之前，被拒绝时 journal 与用户数据
	// 都不会被改动。
	//
	// 已知残留：混合批次（permanent 同时删掉文件型与 SQLite 型会话）因为
	// len(j.SQLite) > 0 会放行，被删掉的文件正文不可恢复，索引还原后可能留下
	// 幽灵条目。要精确判定得让 Delete 在 journal 里记录「永久删除的文件正文数」，
	// 那是 schema 改动，留到后续单独做，这里不投机。
	if j.Mode != ModeTrash && len(j.SQLite) == 0 {
		return UndoResult{Errors: []string{}},
			fmt.Errorf("clean: 批次 %s 的删除模式为 %q 且没有数据库备份，正文已永久删除；只有 trash 批次或带数据库备份的永久删除批次可以撤销", batchID, j.Mode)
	}
	out := UndoResult{Errors: []string{}}
	if j.Undone {
		return out, nil
	}

	// 旧版本写的 journal 没有 home 字段，撤销时无法判断 Orig 是否还在允许的
	// 数据根内。这里必须 fail-closed：缺字段不等于「无需校验」。放在 Undone
	// 之后，是因为已经撤销过的批次没有任何磁盘动作，重复撤销仍应安静成功。
	if strings.TrimSpace(j.Home) == "" {
		return out, fmt.Errorf("clean: 批次 %s 的 journal 是旧格式（缺少 home 字段），无法校验原始路径是否在数据根内；为安全起见已拒绝撤销，请按 journal 的 moved 记录手工恢复", batchID)
	}

	// 数据库备份先于普通索引：两者都是覆盖，但数据库还要额外清 sidecar。
	for i := len(j.SQLite) - 1; i >= 0; i-- {
		entry := j.SQLite[i]
		if entry.Backup == "" {
			continue
		}
		if err := removeSQLiteSidecars(entry.DB); err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("清理数据库 sidecar 失败，已跳过恢复 %s: %v", entry.DB, err))
			continue
		}
		if err := copyFileAtomic(entry.Backup, entry.DB); err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("恢复数据库 %s 失败: %v", entry.DB, err))
			continue
		}
		out.Restored++
	}
	for i := len(j.Indexes) - 1; i >= 0; i-- {
		entry := j.Indexes[i]
		if entry.Backup == "" {
			continue
		}
		if err := copyFileAtomic(entry.Backup, entry.Orig); err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("恢复索引 %s 失败: %v", entry.Orig, err))
			continue
		}
		out.Restored++
	}

	// 废纸篓条目逆序挪回。逆序是为了让「先删正文、后删旁挂目录」的批次
	// 按相反顺序复原，和文件系统里最后状态对得上。
	for i := len(j.Moved) - 1; i >= 0; i-- {
		entry := j.Moved[i]
		if entry.Orig == "" || entry.Trashed == "" {
			out.Errors = append(out.Errors, "journal 里的移动记录不完整，已跳过")
			continue
		}
		// 恢复前重放删除时的越界护栏，并且放在任何文件系统探测之前：删除时
		// checkPathInHome 拦住了越界的 Paths，但 journal 本身是可以被改写的
		// 文件（journal 目录权限、未来的导入/恢复接口）；不校验就
		// MkdirAll + movePath，等于给越界路径开了一条「撤销即任意写」的路。
		// 单条被拒只记 Errors，不影响其它条目的恢复。
		if err := checkPathInHome(j.Home, entry.Orig); err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("拒绝恢复数据根之外的路径 %s: %v", entry.Orig, err))
			continue
		}
		// 「目标已存在」有两种含义，必须分开处理：
		//
		//   - 废纸篓副本也不在了：说明上一次撤销已经把这条挪回去了，现在是一次
		//     重试。这是成功状态，要安静跳过，且绝不能记进 Errors —— 否则上一次
		//     的旧失败会让重试永远带着错误，永远无法收敛到 Undone。
		//   - 副本还在：目标被别的东西占了（用户新建了同名会话，或手工放回了一份），
		//     此时挪回去就是覆盖，必须报错留给用户判断。
		origExists := pathExists(entry.Orig)
		trashedExists := pathExists(entry.Trashed)
		if origExists && !trashedExists {
			continue
		}
		if origExists {
			out.Errors = append(out.Errors, fmt.Sprintf("目标已存在，跳过恢复: %s", entry.Orig))
			continue
		}
		if !trashedExists {
			out.Errors = append(out.Errors, fmt.Sprintf("废纸篓副本不存在: %s", entry.Trashed))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(entry.Orig), 0o755); err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("创建恢复目录失败 %s: %v", entry.Orig, err))
			continue
		}
		if err := movePath(entry.Trashed, entry.Orig); err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("恢复 %s 失败: %v", entry.Orig, err))
			continue
		}
		out.Restored++
	}

	// 只有整批成功才标记 undone。部分失败时保持 false：
	//
	// 失败的条目（磁盘满、权限、目标被占）在用户修好之后恰恰是可以重试的，而已经
	// 成功的条目会在重试时被上面的「已恢复」/「副本不存在」分支安全跳过，所以重试
	// 不会重复搬文件、也不会覆盖用户的新文件。如果照样置 true，Batches() 会把批次
	// 当成「已撤销」而收掉撤销入口，用户就只能手工搬文件。
	//
	// 无论成功与否都要写回 journal：成功时要落盘 Undone，失败时也要把这一次的
	// 部分进度留在磁盘上，下一次重试才有据可依。失败项通过 Errors 完整交还。
	if len(out.Errors) == 0 {
		j.Undone = true
	}
	if err := c.writeJournal(j); err != nil {
		return out, fmt.Errorf("clean: 恢复完成但写回 journal 失败: %w", err)
	}
	return out, nil
}

// Batches 读取 journal 目录，按时间倒序返回批次摘要。
//
// 坏 journal 直接跳过而不是报错：用户可能手工清过目录、旧版本可能留下半截文件，
// 历史列表少一条远好过整个「最近删除」页空白。
func (c *Cleaner) Batches(limit int) ([]Batch, error) {
	entries, err := os.ReadDir(c.opts.JournalDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Batch{}, nil
		}
		return nil, err
	}

	out := make([]Batch, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(c.opts.JournalDir, name))
		if err != nil {
			continue
		}
		var j journal
		if err := json.Unmarshal(data, &j); err != nil {
			continue
		}
		if j.BatchID == "" {
			j.BatchID = strings.TrimSuffix(name, ".json")
		}
		agents := j.Agents
		if agents == nil {
			agents = []string{}
		}
		out = append(out, Batch{
			ID:     j.BatchID,
			At:     j.At,
			Count:  j.Count,
			Freed:  j.Freed,
			Agents: agents,
			// 只有 trash 模式的批次能对用户承诺「撤销」。
			// permanent 模式虽然还能还原索引备份，但正文已经没了 ——
			// 让 UI 显示一个只能恢复一半的撤销按钮，比不给按钮更糟。
			Undoable: !j.Undone && j.Mode == ModeTrash,
		})
	}

	sort.SliceStable(out, func(i, k int) bool {
		if !out[i].At.Equal(out[k].At) {
			return out[i].At.After(out[k].At)
		}
		return out[i].ID > out[k].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
