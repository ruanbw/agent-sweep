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

// Delete 删除一批会话。
//
// 流程固定为：校验模式 -> 预留批次目录（同时确定 batchID）-> 删正文 -> 删 SQLite 行
// -> 裁索引 -> 落 journal。任何单条失败都只进 Result.Errors，不影响其余会话；
// 只有「批次目录建不出来」或「journal 写不进去」这种会让整批失去可撤销性的问题
// 才返回 error。
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
		if c.deleteFileSession(s, home, trashRoot, trashErr, j, &res) {
			res.Count++
			addDeletedID(deleted, s)
			addAgent(agents, s, spec)
		}
	}

	if len(sqliteSessions) > 0 {
		c.deleteSQLiteSessions(spec, home, sqliteSessions, trashRoot, trashErr, j, &res, namer, deleted, agents)
	}

	if len(deleted) > 0 {
		c.trimIndexes(spec, home, deleted, j, &res, namer)
	}

	j.Count = res.Count
	j.Freed = res.Freed
	j.Agents = sortedKeys(agents)
	if err := c.writeJournal(j); err != nil {
		return res, fmt.Errorf("clean: 写 journal 失败，本批次无法撤销: %w", err)
	}
	return res, nil
}

// deleteFileSession 处理一条非 SQLite 会话。
//
// 先校验全部路径再动手，而不是边校验边删：一条会话可能同时带正文和旁挂目录，
// 如果第一个删了、第二个越界被拒，用户会得到一个半删状态，比整条拒绝更难解释。
func (c *Cleaner) deleteFileSession(
	s session.Session,
	home, trashRoot string,
	trashErr error,
	j *journal,
	res *Result,
) bool {
	if len(s.Paths) == 0 {
		res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: "没有可删除的路径"})
		return false
	}
	if home == "" {
		res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: "数据根为空，无法做路径越界检查"})
		return false
	}

	bad := false
	for _, path := range s.Paths {
		if err := checkPathInHome(home, path); err != nil {
			res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: err.Error()})
			bad = true
		}
	}
	if bad {
		return false
	}

	ok := true
	for _, path := range s.Paths {
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
			continue
		}

		size := fsx.Size(path)
		if err := os.RemoveAll(path); err != nil {
			res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: err.Error()})
			ok = false
			continue
		}
		res.Freed += size
	}
	return ok
}

// deleteSidecars 删除 SQLite 型会话的旁挂文件 / 目录。
//
// 与 deleteFileSession 分开：那边「任一路径失败 = 整条会话不删」，
// 这里行已经删了，旁挂目录失败只能记账 —— 语义不同，不能复用。
func (c *Cleaner) deleteSidecars(
	s session.Session,
	home, trashRoot string,
	trashErr error,
	j *journal,
	res *Result,
) {
	for _, path := range s.Paths {
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
			continue
		}
		size := fsx.Size(path)
		if err := os.RemoveAll(path); err != nil {
			res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: err.Error()})
			continue
		}
		res.Freed += size
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

// checkPathInHome 是删除路径的硬护栏。
//
// 会话 Paths 来自扫描结果，理论上都在数据根下；但自定义适配器、被改过的清单、
// 甚至上游解析 bug 都可能塞进 `../../Documents`。这里只看词法上的 Rel 结果，
// 不跟符号链接：删除一个指向外部的链接本身只动链接，不会顺着它删目标。
func checkPathInHome(home, path string) error {
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
