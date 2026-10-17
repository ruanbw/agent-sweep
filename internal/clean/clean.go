// Package clean 负责「真正删掉」这一步：正文、索引、SQLite 行，以及让用户后悔时有路可退。
//
// 这里最重要的设计是把备份做成删除的前置条件，而不是删完再补一份。索引和 SQLite
// 库一旦被改，原始内容就只存在于备份里；如果先删后备份，备份失败时用户面对的是
// 一个正文没了、索引还指着幽灵会话的 Agent。所以每一项都是「先能备份，才允许动」。
//
// 另一个取舍是 trash 与 permanent 的语义必须分开：进废纸篓只是挪了位置，磁盘并未
// 释放，此时把大小算进 Freed 是在向用户谎报战果。只有真正 os.RemoveAll 和 SQLite
// VACUUM 收缩掉的空间才计入 Freed。
package clean

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/fsx"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// 两种删除模式。字符串与 HTTP 契约里的 prefs.deleteMode 保持一致。
const (
	ModeTrash     = "trash"
	ModePermanent = "permanent"
)

// Options 是 Cleaner 的构造参数。
type Options struct {
	Mode       string // "trash" | "permanent"
	BackupDir  string // 备份根目录，调用方传 ~/Library/Application Support/AgentSweep/backups
	JournalDir string // ~/Library/Application Support/AgentSweep/journal
	Now        func() time.Time
}

// ItemError 是单条会话上的非致命错误。
//
// 用 UID 而不是文件名：UI 是按 UID 勾选的，用户需要知道「哪一行没删掉」，
// 而不是收到一句「有 3 个错误」。
type ItemError struct {
	UID     string `json:"uid"`
	Message string `json:"message"`
}

// Result 一次删除的汇总。
type Result struct {
	BatchID string      `json:"batchId"`
	Freed   int64       `json:"freed"`
	Count   int         `json:"count"`
	Errors  []ItemError `json:"errors"`
}

// UndoResult 一次撤销的汇总。
type UndoResult struct {
	Restored int      `json:"restored"`
	Errors   []string `json:"errors"`
}

// Batch 是 Batches() 暴露给 UI 的批次摘要。
type Batch struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	Count    int       `json:"count"`
	Freed    int64     `json:"freed"`
	Agents   []string  `json:"agents"`
	Undoable bool      `json:"undoable"`
}

// Cleaner 是无状态的删除器；所有可变状态都落在备份目录与 journal 里。
type Cleaner struct {
	opts Options
}

// opMu 串行化删除与撤销。
//
// 删除会同时改正文、索引和 SQLite，撤销则按 journal 反向改回来；两者交错执行会
// 产生谁都没法解释的中间态。桌面应用里删除是低频操作，串行化的代价可以忽略。
// 它同时保证同一秒内的 batchID 分配不会互相踩。
var opMu sync.Mutex

// New 构造 Cleaner。空字段走安全默认值：
//   - Mode 默认 trash（宁可让用户去废纸篓捞，也不要一上来就不可逆）
//   - Now 默认 time.Now
//   - BackupDir / JournalDir 默认落在 AgentSweep 的 AppDir 下
func New(opts Options) *Cleaner {
	if opts.Mode == "" {
		opts.Mode = ModeTrash
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	appDir := catalog.AppDir()
	if opts.BackupDir == "" {
		opts.BackupDir = filepath.Join(appDir, "backups")
	}
	if opts.JournalDir == "" {
		opts.JournalDir = filepath.Join(appDir, "journal")
	}
	return &Cleaner{opts: opts}
}

// journalWriter 负责在删除过程中逐步把 journal 原子写到磁盘。
//
// 这里刻意不做「最后写一次」：进程可能在删完正文、改完索引、VACUUM 完数据库之后
// 被 kill，而扁平化的备份文件名反推不出原路径。所以每个破坏性动作一完成就整份
// 重写 journal；几十条会话的 O(n²) 序列化换来的，是任意时刻磁盘上都有一份能撤销、
// 能审计的账本。
type journalWriter struct {
	c      *Cleaner
	j      *journal
	res    *Result
	agents map[string]bool

	// 最后一次成功落盘的条目数，只用于把写盘失败的错误说清楚。
	onDisk journalSnapshot
}

type journalSnapshot struct {
	moved, sqlite, indexes int
}

func newJournalWriter(c *Cleaner, j *journal, res *Result, agents map[string]bool) *journalWriter {
	w := &journalWriter{c: c, j: j, res: res, agents: agents}
	// deleteSidecars 由 sqlite.go 调用，签名不能改；它只能通过 journal 上的
	// 回调拿到落盘能力。
	j.persist = w.save
	return w
}

// save 刷新派生字段后原子重写 journal。
//
// Count / Freed / Agents 不是调用点手工维护的，而是每次落盘前从 Result 与
// agents 集合重新算出来，避免「某次追加忘了同步摘要」这种只在中途崩溃时
// 才暴露的偏差。
func (w *journalWriter) save() error {
	w.j.Count = w.res.Count
	w.j.Freed = w.res.Freed
	w.j.Agents = sortedKeys(w.agents)
	if err := w.c.writeJournal(w.j); err != nil {
		return err
	}
	w.onDisk = journalSnapshot{
		moved:   len(w.j.Moved),
		sqlite:  len(w.j.SQLite),
		indexes: len(w.j.Indexes),
	}
	return nil
}

// failure 把「删到一半 journal 写不进去」的边界说清楚：已经计入 Result 的条数、
// 磁盘上最后一份完整 journal 覆盖到哪、内存里还有多少条没落盘。
func (w *journalWriter) failure(cause error) error {
	return fmt.Errorf(
		"clean: 写 journal 失败，已停止后续删除（batch %s；已成功删除 %d 条会话；"+
			"磁盘上最后一份完整 journal 含 %d 项移动、%d 项数据库、%d 项索引；"+
			"此后未落盘的有 %d 项移动、%d 项数据库、%d 项索引）: %w",
		w.j.BatchID, w.res.Count,
		w.onDisk.moved, w.onDisk.sqlite, w.onDisk.indexes,
		len(w.j.Moved)-w.onDisk.moved,
		len(w.j.SQLite)-w.onDisk.sqlite,
		len(w.j.Indexes)-w.onDisk.indexes,
		cause)
}

// Delete 删除一批会话。
//
// 流程固定为：校验模式 -> 预留批次目录（同时确定 batchID）-> 先落一份空 journal
// -> 删正文 -> 删 SQLite 行 -> 裁索引 -> 收尾重写 journal。journal 必须在任何
// 破坏性动作之前就存在，并且每完成一次动作就原子重写一次：进程中途被 kill 时，
// 磁盘上至少有一份能说明「已经动了哪些路径」的账本。
//
// 任何单条失败都只进 Result.Errors，不影响其余会话；只有「批次目录建不出来」或
// 「journal 写不进去」这种会让整批失去可撤销性的问题才返回 error。后者会立即
// 停手——继续删只会扩大无法撤销的范围。
func (c *Cleaner) Delete(spec catalog.Spec, home string, sessions []session.Session) (Result, error) {
	if c.opts.Mode != ModeTrash && c.opts.Mode != ModePermanent {
		return Result{Errors: []ItemError{}},
			fmt.Errorf("clean: 未知删除模式 %q（只支持 trash 或 permanent）", c.opts.Mode)
	}

	opMu.Lock()
	defer opMu.Unlock()

	now := c.opts.Now()
	batchID, err := c.reserveBatch(now)
	if err != nil {
		return Result{Errors: []ItemError{}}, err
	}

	j := &journal{
		BatchID: batchID,
		At:      now,
		Mode:    c.opts.Mode,
		Moved:   []journalMoved{},
		Indexes: []journalBackup{},
		SQLite:  []journalDB{},
		Agents:  []string{},
	}
	res := Result{BatchID: batchID, Errors: []ItemError{}}
	namer := &backupNamer{}
	deleted := map[string]bool{} // 正文确实消失了的 ID，索引只裁这些
	agents := map[string]bool{}

	writer := newJournalWriter(c, j, &res, agents)
	if err := writer.save(); err != nil {
		return res, fmt.Errorf("clean: 写初始 journal 失败，未开始删除（batch %s）: %w", batchID, err)
	}

	var fileSessions, sqliteSessions []session.Session
	for _, s := range sessions {
		if s.Storage == session.StorageSQLite {
			sqliteSessions = append(sqliteSessions, s)
		} else {
			fileSessions = append(fileSessions, s)
		}
	}

	trashRoot := ""
	var trashErr error
	if c.opts.Mode == ModeTrash {
		userHome, err := os.UserHomeDir()
		if err != nil {
			trashErr = fmt.Errorf("找不到用户主目录: %w", err)
		} else {
			trashRoot = filepath.Join(userHome, ".Trash", "AgentSweep", batchID)
			if err := os.MkdirAll(trashRoot, 0o700); err != nil {
				trashErr = fmt.Errorf("创建废纸篓目录失败: %w", err)
			}
		}
	}

	for _, s := range fileSessions {
		ok, err := c.deleteFileSession(s, home, trashRoot, trashErr, j, &res, writer)
		if ok {
			// 先记账再落盘：即使紧接着的写盘失败，Result 也能准确告诉上层
			// 这条会话已经删掉了。
			res.Count++
			addDeletedID(deleted, s)
			addAgent(agents, s, spec)
		}
		if err != nil {
			return res, writer.failure(err)
		}
		if ok {
			if err := writer.save(); err != nil {
				return res, writer.failure(err)
			}
		}
	}

	if len(sqliteSessions) > 0 {
		c.deleteSQLiteSessions(spec, home, sqliteSessions, trashRoot, trashErr, j, &res, namer, deleted, agents)
		// deleteSidecars 在 sqlite.go 里被调用，无法返回 error；它把第一次
		// 写盘失败记在 journal 上，这里统一停手。
		if j.persistErr != nil {
			return res, writer.failure(j.persistErr)
		}
		if err := writer.save(); err != nil {
			return res, writer.failure(err)
		}
	}

	if len(deleted) > 0 {
		c.trimIndexes(spec, home, deleted, j, &res, namer)
		if j.persistErr != nil {
			return res, writer.failure(j.persistErr)
		}
		if err := writer.save(); err != nil {
			return res, writer.failure(err)
		}
	}

	if err := writer.save(); err != nil {
		return res, writer.failure(err)
	}
	return res, nil
}

// deleteFileSession 处理一条非 SQLite 会话。
//
// 先校验全部路径再动手，而不是边校验边删：一条会话可能同时带正文和旁挂目录，
// 如果第一个删了、第二个越界被拒，用户会得到一个半删状态，比整条拒绝更难解释。
//
// 每成功移走/删掉一个路径就立刻落一次 journal：崩溃发生在这里时，已经离开原位的
// 文件仍然在账本里。写盘失败返回 error，调用方必须停止整批删除。
func (c *Cleaner) deleteFileSession(
	s session.Session,
	home, trashRoot string,
	trashErr error,
	j *journal,
	res *Result,
	writer *journalWriter,
) (bool, error) {
	if len(s.Paths) == 0 {
		res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: "没有可删除的路径"})
		return false, nil
	}
	if home == "" {
		res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: "数据根为空，无法做路径越界检查"})
		return false, nil
	}

	bad := false
	for _, path := range s.Paths {
		if err := checkPathInHome(home, path); err != nil {
			res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: err.Error()})
			bad = true
		}
	}
	if bad {
		return false, nil
	}

	ok := true
	for i, path := range s.Paths {
		if c.opts.Mode == ModeTrash {
			if trashErr != nil {
				res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: trashErr.Error()})
				ok = false
				continue
			}
			dest, err := moveToTrash(path, trashRoot)
			if err != nil {
				res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: err.Error()})
				ok = false
				continue
			}
			j.Moved = append(j.Moved, journalMoved{Orig: path, Trashed: dest})
			if err := writer.save(); err != nil {
				// 只有已经处理完所有路径时，这条会话才算完整删除；
				// 否则调用方不能把它计入 Result.Count。
				return ok && i == len(s.Paths)-1, err
			}
			continue
		}

		size := fsx.Size(path)
		if err := os.RemoveAll(path); err != nil {
			res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: err.Error()})
			ok = false
			continue
		}
		res.Freed += size
		if err := writer.save(); err != nil {
			return ok && i == len(s.Paths)-1, err
		}
	}
	return ok, nil
}

// deleteSidecars 删除 SQLite 型会话的旁挂文件 / 目录。
//
// 与 deleteFileSession 分开：那边「任一路径失败 = 整条会话不删」，
// 这里行已经删了，旁挂目录失败只能记账 —— 语义不同，不能复用。
//
// 签名由 sqlite.go 的调用点固定，不能返回 error；journal 写盘失败记录在
// j.persistErr 上，Delete 在 SQLite 阶段结束后检查并停止后续破坏。
func (c *Cleaner) deleteSidecars(
	s session.Session,
	home, trashRoot string,
	trashErr error,
	j *journal,
	res *Result,
) {
	for _, path := range s.Paths {
		// 上一轮已经写 journal 失败：不再移走任何东西，等 Delete 收尾报错。
		if j.persistErr != nil {
			return
		}
		if path == "" {
			continue
		}
		if err := checkPathInHome(home, path); err != nil {
			res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: err.Error()})
			continue
		}
		if c.opts.Mode == ModeTrash {
			if trashErr != nil {
				res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: trashErr.Error()})
				continue
			}
			dest, err := moveToTrash(path, trashRoot)
			if err != nil {
				res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: err.Error()})
				continue
			}
			j.Moved = append(j.Moved, journalMoved{Orig: path, Trashed: dest})
			if err := j.save(); err != nil {
				j.persistErr = err
				return
			}
			continue
		}
		size := fsx.Size(path)
		if err := os.RemoveAll(path); err != nil {
			res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: err.Error()})
			continue
		}
		res.Freed += size
		if err := j.save(); err != nil {
			j.persistErr = err
			return
		}
	}
}

// reserveBatch 分配 batchID 并创建对应的备份目录。
//
// 用「创建目录」而不是「查一下再创建」来占位：Mkdir 的 EEXIST 是原子的，即使
// 另一个进程同时在同一秒删东西，也只有一个能拿到基础 ID，另一个自然落到 -2。
// 目录建成后 batchID 就确定了；备份失败最多是某一项被跳过，不会回头改 ID。
func (c *Cleaner) reserveBatch(now time.Time) (string, error) {
	if err := os.MkdirAll(c.opts.BackupDir, 0o700); err != nil {
		return "", fmt.Errorf("clean: 创建备份根目录失败: %w", err)
	}
	if err := os.MkdirAll(c.opts.JournalDir, 0o700); err != nil {
		return "", fmt.Errorf("clean: 创建 journal 目录失败: %w", err)
	}

	base := now.Format("20060102-150405")
	for n := 1; n < 10000; n++ {
		id := base
		if n > 1 {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		if fsx.Exists(c.journalPath(id)) {
			continue
		}
		err := os.Mkdir(filepath.Join(c.opts.BackupDir, id), 0o700)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("clean: 创建批次备份目录失败: %w", err)
		}
	}
	return "", errors.New("clean: 同一秒内批次过多，无法分配 batchID")
}

// addDeletedID 让索引裁剪同时认 ID 与 UID。
//
// 大多数 Agent 的索引存的是自己的 sessionId，但自定义适配器里也可能写 UID；
// 两者都放进集合，匹配时不会误伤（UID 几乎不可能与普通 ID 撞上）。
func addDeletedID(set map[string]bool, s session.Session) {
	if s.ID != "" {
		set[s.ID] = true
	}
	if s.UID != "" {
		set[s.UID] = true
	}
}

func addAgent(set map[string]bool, s session.Session, spec catalog.Spec) {
	agent := s.AgentID
	if agent == "" {
		agent = spec.ID
	}
	if agent != "" {
		set[agent] = true
	}
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// checkPathInHome 是删除路径的硬护栏，分两层。
//
// 会话 Paths 来自扫描结果，理论上都在数据根下；但自定义适配器、被改过的清单、
// 甚至上游解析 bug 都可能塞进 `../../Documents`。第一层只看词法上的 Rel 结果，
// 挡住这种显式逃逸。
//
// 第二层在真正解析文件系统之后再判一次：词法上 `home/projects/s1.jsonl` 确实在
// home 内，但如果 `home/projects` 是指到外部的符号链接，os.RemoveAll / moveToTrash
// 会由内核顺着它落到数据根之外。真实路径校验只解析 home 与目标路径的父目录，
// 最后一段故意不解析 —— 删除一个指向外部的链接本身只动链接，不跟目标，所以
// 「最后一段是 symlink」仍然允许。
func checkPathInHome(home, path string) error {
	if err := checkPathInHomeLexical(home, path); err != nil {
		return err
	}
	return checkPathInHomeReal(home, path)
}

// checkPathInHomeLexical 是只做字符串运算的包含性判断。
//
// 它单独成函数，是因为真实路径层依赖文件系统现状，而这一层在路径不存在时仍然
// 要给出结论，两者失败模式不同，测试也需要分开覆盖。
func checkPathInHomeLexical(home, path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("拒绝删除空路径")
	}
	rel, err := filepath.Rel(home, path)
	if err != nil {
		return fmt.Errorf("路径 %s 无法相对数据根 %s 计算: %w", path, home, err)
	}
	if filepath.IsAbs(rel) {
		return fmt.Errorf("拒绝删除数据根之外的路径: %s", path)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("拒绝删除数据根之外的路径: %s", path)
	}
	if rel == "." {
		// Rel 为 "." 意味着用户选中了数据根本身。这不是「一条会话」，
		// 删除它等于把整个 Agent 抹掉，必须拦住。
		return fmt.Errorf("拒绝删除数据根本身: %s", path)
	}
	return nil
}

// checkPathInHomeReal 用解析掉符号链接后的真实路径再做一次包含性判断。
//
// 先 EvalSymlinks(home)，再 EvalSymlinks(待删路径的父目录)，最后把原 basename
// 拼回去：父目录的中间层 symlink 会被展开，而最后一段保持原样，所以删链接本身
// 依旧安全。
//
// EvalSymlinks 对不存在的路径会返回错误，而「删除前路径已经不存在」是正常情况
// （用户可能刚手动清过，或上一步已删掉），此时应退回词法层的结论，不能把
// 「不存在」当成越界报错。home 本身不存在同理。
func checkPathInHomeReal(home, path string) error {
	if strings.TrimSpace(home) == "" {
		// 词法层已经覆盖了空 home 的行为，这里不再重复判断。
		return nil
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("数据根 %s 的真实路径解析失败: %w", home, err)
	}

	clean := filepath.Clean(path)
	realParent, err := filepath.EvalSymlinks(filepath.Dir(clean))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("路径 %s 的真实父目录解析失败: %w", path, err)
	}
	target := filepath.Join(realParent, filepath.Base(clean))

	rel, err := filepath.Rel(realHome, target)
	if err != nil {
		return fmt.Errorf("路径 %s 无法相对数据根 %s 计算: %w", path, home, err)
	}
	if filepath.IsAbs(rel) ||
		rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("拒绝删除数据根之外的路径（符号链接解析后为 %s）: %s", target, path)
	}
	return nil
}

// resolveInHome 把 spec 里的相对路径（索引库、SQLite 库）解析到数据根内。
//
// 和会话路径同一套护栏：spec 可以来自用户自定义适配器，不能假设它一定安全。
func resolveInHome(home, rel string) (string, error) {
	if home == "" {
		return "", errors.New("数据根为空")
	}
	if rel == "" {
		return "", errors.New("路径为空")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("拒绝使用绝对路径 %s", rel)
	}
	path := filepath.Join(home, rel)
	if err := checkPathInHome(home, path); err != nil {
		return "", err
	}
	return path, nil
}
