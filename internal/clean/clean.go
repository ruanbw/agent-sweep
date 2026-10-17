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
		// 记下这次删除用的数据根：撤销时 journal 里的 Orig 必须重新过一遍
		// checkPathInHome，否则一份可被改写的 journal 就能让应用把文件搬到
		// 任意位置（R10）。旧 journal 没有这个字段，Undo 会拒绝恢复。
		Home:    home,
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
// trash 模式下「任一条失败 = 整条会话失败」还不够：只逐条 move、失败后继续，
// 会留下「前几条已在废纸篓、后几条还在原位」的中间态；UI 按整条失败展示，
// 用户重新删除时前几条的源路径已经不存在，于是永远删不干净（R11）。这里用
// 两道闸把整条会话做成一个事务：
//
//  1. 主方案是移动前预检（preflightTrashMove / preflightTrashRoot）：源存在、
//     父目录可写可进入、废纸篓目录可用。预检不写磁盘，失败时整条会话原样不动，
//     也不会像回滚那样产生二次破坏；代价是预检与真正的 rename 之间存在 TOCTOU
//     窗口，覆盖不了磁盘写满、并发改动这类运行期错误。
//  2. 兜底是失败回滚（rollbackTrashSession）：预检通过后仍可能在移动第 i 条时
//     失败，此时把本次会话刚挪走的条目按相反顺序挪回去。回滚只是尽力而为，
//     哪些条目最终留在废纸篓会逐条反映到 j.Moved，撤销账本不会说谎。
//
// 目标重名不需要预检：moveToTrash 自己用 -2/-3 递增避让；预检若先占一个名字，
// 反而会和随后的真正移动之间制造新的竞态。
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

	if c.opts.Mode == ModeTrash {
		return trashFileSession(s, trashRoot, trashErr, j, res, writer)
	}

	// permanent 模式语义保持不变：没有废纸篓可回滚，逐条 RemoveAll，
	// 失败的路径逐条记错，不影响其余路径。
	ok := true
	for i, path := range s.Paths {
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

// trashFileSession 以「整条会话」为事务边界把 s.Paths 挪进废纸篓。
//
// 返回 false 表示这条会话没有完整挪走（Result.Count 不应 +1）；返回 error 只用于
// journal 写盘失败，调用方据此停止整批删除。
func trashFileSession(
	s session.Session,
	trashRoot string,
	trashErr error,
	j *journal,
	res *Result,
	writer *journalWriter,
) (bool, error) {
	if trashErr != nil {
		res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: trashErr.Error()})
		return false, nil
	}
	if err := preflightTrashRoot(trashRoot); err != nil {
		res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: err.Error()})
		return false, nil
	}

	// 预检全部路径，任何一条不合格都在移动之前整条拒绝：这是「要么整条挪走、
	// 要么原样不动」的主方案，也避免用户重新删除时面对已不存在的源路径。
	bad := false
	for _, path := range s.Paths {
		if err := preflightTrashMove(path); err != nil {
			res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: err.Error()})
			bad = true
		}
	}
	if bad {
		return false, nil
	}

	// start 之前的 Moved 条目属于先前的会话；回滚只能碰本次追加的区间。
	start := len(j.Moved)
	for i, path := range s.Paths {
		dest, err := moveToTrash(path, trashRoot)
		if err != nil {
			res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: err.Error()})
			// 预检没预测到的运行期失败（竞态、磁盘写满等）：尽力把本次会话
			// 已经挪走的条目搬回去，并要求 journal 与磁盘现状一致。
			if rollbackTrashSession(s.UID, j, start, res) {
				if saveErr := writer.save(); saveErr != nil {
					return false, saveErr
				}
			}
			return false, nil
		}
		j.Moved = append(j.Moved, journalMoved{Orig: path, Trashed: dest})
		if err := writer.save(); err != nil {
			// 只有已经处理完所有路径时，这条会话才算完整删除；
			// 否则调用方不能把它计入 Result.Count。
			return i == len(s.Paths)-1, err
		}
	}
	return true, nil
}

// rollbackTrashSession 尽力把本次会话已经挪进废纸篓的条目挪回原位。
//
// 只在预检没预测到的运行期失败后调用，因此它自己也可能失败；这里的原则是
// 「不制造二次破坏，且 j.Moved 必须与磁盘一致」：
//   - 副本已不在废纸篓：从 j.Moved 删掉，否则撤销会去搬一个不存在的文件；
//   - 原位置已被占用：绝不覆盖（覆盖等于拿废纸篓副本覆盖用户新数据），
//     副本确实还在废纸篓里，保留记录并由 Undo 的「目标已存在」分支处理；
//   - 搬回成功：副本已经不在了，从 j.Moved 删掉。
//
// 返回值表示是否从 j.Moved 中删掉了条目；调用方据此决定是否需要重新落盘。
func rollbackTrashSession(uid string, j *journal, start int, res *Result) bool {
	pruned := false
	for i := len(j.Moved) - 1; i >= start; i-- {
		entry := j.Moved[i]
		if entry.Orig == "" || entry.Trashed == "" {
			// 记录本身不完整，没有可靠的原路径可搬；保留原地不动。
			continue
		}
		if !pathExists(entry.Trashed) {
			j.Moved = append(j.Moved[:i], j.Moved[i+1:]...)
			pruned = true
			continue
		}
		if pathExists(entry.Orig) {
			res.Errors = append(res.Errors, ItemError{
				UID:     uid,
				Message: fmt.Sprintf("回滚 %s 失败：原位置已被占用，废纸篓副本保留在 %s", entry.Orig, entry.Trashed),
			})
			continue
		}
		if err := movePath(entry.Trashed, entry.Orig); err != nil {
			res.Errors = append(res.Errors, ItemError{
				UID:     uid,
				Message: fmt.Sprintf("回滚 %s 失败，废纸篓副本保留在 %s: %v", entry.Orig, entry.Trashed, err),
			})
			continue
		}
		j.Moved = append(j.Moved[:i], j.Moved[i+1:]...)
		pruned = true
	}
	return pruned
}

// preflightTrashRoot 确认本批次的废纸篓目录可写。
//
// Delete 已经 MkdirAll 过；这里再查一次是因为「目录建得出来」不等于「目录里
// 放得进东西」。检查放在逐条预检之前：根目录不可写时没有必要为每条路径重复
// 报一遍同样的错。
func preflightTrashRoot(trashRoot string) error {
	info, err := os.Stat(trashRoot)
	if err != nil {
		return fmt.Errorf("废纸篓目录不可访问 %s: %w", trashRoot, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("废纸篓路径不是目录: %s", trashRoot)
	}
	if perm := info.Mode().Perm(); perm&0o222 == 0 {
		return fmt.Errorf("废纸篓目录不可写 %s（权限 %04o），整条会话保持不变", trashRoot, perm)
	}
	return nil
}

// preflightTrashMove 在真正 rename 之前判断一条路径是否可移动。
//
// 只做只读探测，不写磁盘：这是它比「先挪、失败再回滚」更安全的地方 —— 拒绝时
// 用户数据连位置都没变，也没有第二次 move 的失败风险。
//
// 可写性用权限位判断，而不是在用户的数据目录里写一个探针文件：探针本身就会
// 制造一次写入，在只读/权限异常时还可能留下残留，违背「整条会话原样不动」。
// 代价是它偏保守（ACL 授予的写权限看不到），但误拒只是让用户看到一条错误，
// 不会删掉任何东西，符合这里「宁可不动」的取舍。
//
// 目标重名不在这里处理：moveToTrash 会在真正落位时用 -2/-3 递增避让，预检
// 提前占名反而会与随后的移动之间制造竞态。
func preflightTrashMove(path string) error {
	if _, err := os.Lstat(path); err != nil {
		return fmt.Errorf("源路径不存在或不可访问 %s: %w", path, err)
	}
	parent := filepath.Dir(filepath.Clean(path))
	info, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("父目录不可访问 %s: %w", parent, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("父目录不是目录: %s", parent)
	}
	perm := info.Mode().Perm()
	if perm&0o222 == 0 {
		return fmt.Errorf("父目录不可写 %s（权限 %04o），整条会话保持不变", parent, perm)
	}
	if perm&0o111 == 0 {
		return fmt.Errorf("父目录不可进入 %s（权限 %04o），整条会话保持不变", parent, perm)
	}
	return nil
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
