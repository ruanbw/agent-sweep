package clean

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/fsx"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// sqlitePlan 是一个待处理的库文件。
//
// 同一份 spec 里可能有多个 Store 指向同一个 DB（不同表），备份只能做一次；
// 按库归并而不是按 Store 循环，是为了避免「备份 A 表、删 A 表、再备份一次同一个库」
// 这种把备份做在修改之后的顺序错误。
type sqlitePlan struct {
	path      string
	stores    []catalog.Store
	ids       []string // 落到这个库的会话 ID（按 DBPath 分组后）
	backup    string
	backupErr error
	deleteErr error
	vacuumErr error
	rows      []journalRow
}

// deleteSQLiteSessions 删除 Storage=="sqlite" 的会话行。
//
// 顺序被刻意固定为「备份整个库 -> 一个事务里删主表和子表 -> VACUUM」：
//   - 备份放在最前面，是因为删行不可撤销，而 VACUUM 之后旧页可能被复用；
//   - 主表与子表放在同一事务，避免出现「主行没了、消息还在」的孤儿数据；
//   - VACUUM 单独执行（SQLite 不允许在事务里 VACUUM），释放量按执行前后文件大小差算。
//
// 库按 session.DBPath 分组：DB 声明成 glob 时一个 Agent 会有多个库，每个库只
// 备份/删一次，ID 集合取组内会话。没有 DBPath 的旧会话（旧 journal / 旧内存）
// 退回「按 Store 声明的单路径」老逻辑，保证既有行为不变。
func (c *Cleaner) deleteSQLiteSessions(
	spec catalog.Spec,
	home string,
	sessions []session.Session,
	trashRoot string,
	trashErr error,
	j *journal,
	res *Result,
	namer *backupNamer,
	deleted map[string]bool,
	agents map[string]bool,
) {
	var plans []*sqlitePlan
	byPath := map[string]*sqlitePlan{}
	warnedFallback := map[string]bool{}
	// sessionPlans 记录每条会话落到哪几个库：错误归属与「这条是否算删成功」
	// 都按它判断，而不是像旧实现那样对每条会话遍历全部 plan。
	sessionPlans := map[string][]*sqlitePlan{}
	var legacy []session.Session
	valid := make([]session.Session, 0, len(sessions))

	planFor := func(path string) *sqlitePlan {
		if plan := byPath[path]; plan != nil {
			return plan
		}
		plan := &sqlitePlan{path: path}
		byPath[path] = plan
		plans = append(plans, plan)
		return plan
	}
	attach := func(s session.Session, plan *sqlitePlan) {
		if !containsPlan(sessionPlans[s.UID], plan) {
			sessionPlans[s.UID] = append(sessionPlans[s.UID], plan)
		}
		if !containsID(plan.ids, s.ID) {
			plan.ids = append(plan.ids, s.ID)
		}
	}

	for _, s := range sessions {
		if s.ID == "" {
			res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: "会话缺少 ID，无法定位 SQLite 行"})
			continue
		}
		valid = append(valid, s)
		if s.DBPath == "" {
			legacy = append(legacy, s)
			continue
		}
		// DBPath 来自扫描结果，理论上一定在数据根内；这里再校验一次，因为
		// journal/内存里的会话对象不是可信输入（撤销走的是另一条链，但删除
		// 也不该把任意绝对路径当成库）。
		relative, err := filepath.Rel(home, s.DBPath)
		if err != nil {
			res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: fmt.Sprintf("会话记录的 SQLite 库路径无法解析，已跳过: %v", err)})
			continue
		}
		path, err := resolveInHome(home, filepath.ToSlash(relative))
		if err != nil {
			res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: fmt.Sprintf("会话记录的 SQLite 库路径不安全，已跳过: %v", err)})
			continue
		}
		store, ok := storeForDBPath(spec, home, path)
		if !ok {
			fallback, hasFallback := firstSQLiteStore(spec)
			if !hasFallback {
				res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: "spec 没有声明可用的 SQLite 存储，无法删除该会话"})
				continue
			}
			store = fallback
			if !warnedFallback[path] {
				warnedFallback[path] = true
				res.Errors = append(res.Errors, ItemError{Message: fmt.Sprintf(
					"无法把数据库 %s 匹配到任何 sqlite Store 声明，已回退到 %s 删行", path, store.DB)})
			}
		}
		plan := planFor(path)
		plan.stores = appendStoreOnce(plan.stores, store)
		attach(s, plan)
	}

	if len(legacy) > 0 {
		hasSQLite := false
		for _, store := range spec.Stores {
			if store.Kind != "sqlite" {
				continue
			}
			hasSQLite = true
			// 旧会话不知道具体库：glob 声明下无法判断该删哪个库，只能如实报错，
			// 而不是猜一个路径去删。
			if containsGlobMeta(store.DB) {
				for _, s := range legacy {
					res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: fmt.Sprintf(
						"Store 的 DB 声明 %q 是 glob，但会话没有记录具体库路径，无法删除该行", store.DB)})
				}
				continue
			}
			path, err := resolveInHome(home, store.DB)
			if err != nil {
				for _, s := range legacy {
					res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: fmt.Sprintf("SQLite 库路径不安全，已跳过: %v", err)})
				}
				continue
			}
			plan := planFor(path)
			plan.stores = appendStoreOnce(plan.stores, store)
			for _, s := range legacy {
				attach(s, plan)
			}
		}
		if !hasSQLite {
			for _, s := range legacy {
				res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: "spec 没有声明可用的 SQLite 存储，无法删除该会话"})
			}
		}
	}

	if len(valid) == 0 {
		return
	}
	if len(plans) == 0 {
		// 每条会话在分组阶段都已经记过错误，这里只收尾。
		return
	}

	for _, plan := range plans {
		if len(plan.ids) == 0 {
			continue
		}
		if !fsx.Exists(plan.path) {
			plan.backupErr = fmt.Errorf("库文件不存在: %s", plan.path)
			continue
		}
		backup, err := c.backupSQLiteFile(plan.path, j.BatchID, namer)
		if err != nil {
			plan.backupErr = fmt.Errorf("备份 SQLite 库 %s 失败，已跳过删行: %w", plan.path, err)
			continue
		}
		plan.backup = backup
	}

	for _, plan := range plans {
		// 没备份成功就绝不打开写连接：这是本包最硬的一条纪律。
		if len(plan.ids) == 0 || plan.backupErr != nil {
			continue
		}

		// 在开始删行之前记录库的足迹（主库 + -wal + -shm）：只看主库会把
		// 「删行落进 WAL、VACUUM 缩主库」这两种变化算漏。这是估算 —— Agent
		// 并发写库时数字可能偏大或偏小，UI 不应把它当精确承诺。
		before := sqliteFootprint(plan.path)

		// 先记账、后删行：把「这个库、这些表、这些 ID 将被删」写进 journal 并
		// 落盘，再执行事务。进程如果死在事务提交之后、下一次落盘之前，账本里
		// 已经有这个库的备份与计划，撤销仍能整库覆盖回去。
		planned := plannedPlanRows(plan)
		j.SQLite = append(j.SQLite, journalDB{DB: plan.path, Backup: plan.backup, Deletes: planned})
		if err := j.save(); err != nil {
			popLastJournalDB(j)
			j.persistErr = err
			return
		}
		journalBeforeActionHook()

		db, err := sql.Open("sqlite", sqliteDSN(plan.path, false))
		if err == nil {
			db.SetMaxOpenConns(1)
			err = db.Ping()
		}
		if err != nil {
			plan.deleteErr = err
			if db != nil {
				db.Close()
			}
			// 库根本没被改：撤回刚预写的条目。
			popLastJournalDB(j)
			if saveErr := j.save(); saveErr != nil {
				j.persistErr = saveErr
				return
			}
			continue
		}

		for _, store := range plan.stores {
			rows, err := deleteSQLiteStore(db, store, plan.ids)
			if err != nil {
				plan.deleteErr = err
				break
			}
			plan.rows = append(plan.rows, rows...)
		}
		// 已提交的删除必须在继续之前落进 journal：多表 spec 里第二张表失败时，
		// 第一张表的行已经真没了，不能因为整条计划被判失败就让撤销对它失明。
		// 同时把预写的「计划」换成实际删掉的表/ID —— 不一致说明声明与实际不符，
		// 以实际为准。
		if plan.deleteErr != nil && len(plan.rows) == 0 {
			popLastJournalDB(j)
		} else {
			j.SQLite[len(j.SQLite)-1].Deletes = plan.rows
		}
		if saveErr := j.save(); saveErr != nil {
			j.persistErr = saveErr
			db.Close()
			return
		}
		if plan.deleteErr != nil {
			db.Close()
			continue
		}

		if _, err := db.Exec("VACUUM"); err != nil {
			plan.vacuumErr = err
		}
		db.Close()
		if freed := freedFromFootprint(before, sqliteFootprint(plan.path)); freed > 0 {
			res.Freed += freed
		}
	}

	for _, s := range valid {
		group := sessionPlans[s.UID]
		failed := len(group) == 0 // 分组阶段已经记过原因，不再重复报
		for _, plan := range group {
			if plan.backupErr != nil {
				res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: plan.backupErr.Error()})
				failed = true
				continue
			}
			if plan.deleteErr != nil {
				res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: plan.deleteErr.Error()})
				failed = true
				continue
			}
			if plan.vacuumErr != nil {
				// 行已经删掉了，VACUUM 失败只影响「释放了多少空间」，不该
				// 把这条会话判成删除失败，否则用户会重复删一个已经不存在的行。
				res.Errors = append(res.Errors, ItemError{
					UID:     s.UID,
					Message: fmt.Sprintf("VACUUM %s 失败（行已删除，空间可能未释放）: %v", plan.path, plan.vacuumErr),
				})
			}
		}
		if failed {
			continue
		}
		// 行删掉之后，把旁挂的正文目录也带走（Antigravity 的 brain/<id> 这类）。
		// 失败只记账：行已经没了，留下目录是孤儿数据，但不影响「这条会话已删除」。
		c.deleteSidecars(spec, s, home, trashRoot, trashErr, j, res)
		res.Count++
		addDeletedID(deleted, s)
		addAgent(agents, s, spec)
	}
}

// storeForDBPath 找出 DB 声明（允许 glob）能匹配这个库文件的那条 sqlite Store。
//
// 只做字符串匹配、不碰磁盘：删除时不该为了确认「这条会话归哪个声明」再遍历
// 一次文件系统。匹配不到时调用方回退到第一条 sqlite Store 并记 warning ——
// 声明被改过的老批次仍能删，但用户会看到原因。
func storeForDBPath(spec catalog.Spec, home, dbPath string) (catalog.Store, bool) {
	relative, err := filepath.Rel(home, dbPath)
	if err != nil {
		return catalog.Store{}, false
	}
	rel := filepath.ToSlash(relative)
	for _, store := range spec.Stores {
		if store.Kind != "sqlite" || store.DB == "" {
			continue
		}
		if catalog.MatchGlob(store.DB, rel) {
			return store, true
		}
	}
	return catalog.Store{}, false
}

func containsGlobMeta(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

func firstSQLiteStore(spec catalog.Spec) (catalog.Store, bool) {
	for _, store := range spec.Stores {
		if store.Kind == "sqlite" {
			return store, true
		}
	}
	return catalog.Store{}, false
}

func appendStoreOnce(stores []catalog.Store, store catalog.Store) []catalog.Store {
	for _, existing := range stores {
		if sameStore(existing, store) {
			return stores
		}
	}
	return append(stores, store)
}

func sameStore(a, b catalog.Store) bool {
	return a.Kind == b.Kind && a.DB == b.DB && a.Table == b.Table && a.IDCol == b.IDCol &&
		a.TitleCol == b.TitleCol && a.TimeCol == b.TimeCol && a.CWDCol == b.CWDCol &&
		a.Compress == b.Compress && strings.Join(a.Rows, "\x00") == strings.Join(b.Rows, "\x00")
}

func containsID(ids []string, id string) bool {
	for _, existing := range ids {
		if existing == id {
			return true
		}
	}
	return false
}

func containsPlan(plans []*sqlitePlan, target *sqlitePlan) bool {
	for _, plan := range plans {
		if plan == target {
			return true
		}
	}
	return false
}

// openDSN 拼出写连接（备份 checkpoint / 删行 / VACUUM）的 DSN。
//
// 路径只做 EscapedPath（空格 → %20，`?`、`#`、`%` 也一并转义），绝不把裸路径
// 直接拼进 `file:` URI：modernc 驱动把 `file:` 开头的 DSN 当 URI 解析，而 `?`
// 和 `#` 在 macOS 上是完全合法的文件名字符。裸拼会让 URI 被截断或改变语义 ——
// 轻则打不开（用户看到「0 条会话」或删除失败），重则打开到另一个文件、改动
// 与预期不符的库，这对清理工具是数据安全问题。
//
// 写法与 internal/scan、internal/probe 中的同名逻辑对齐（只转义 path，不用
// url.URL.String() 生成 file:/// 三斜杠形态）；本包各留一份，不跨包导出。
//
// busy_timeout 不是可选项：Agent 自己往往正开着库（Zed、VS Code 都是常驻进程），
// 没有等待就会随机报 database is locked，用户看到的是「有时能删有时不能」。
func openDSN(path string) string {
	escaped := (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()
	return "file:" + escaped + "?_pragma=busy_timeout(2000)"
}

// sqliteDSN 统一 DSN 形态；readonly 只给只读场景用，删除必须可写，绝不能
// 顺手带上 mode=ro。转义由 openDSN 负责，这里只追加只读开关。
func sqliteDSN(path string, readonly bool) string {
	dsn := openDSN(path)
	if readonly {
		dsn += "&mode=ro"
	}
	return dsn
}

// backupSQLiteFile 在复制前先 checkpoint。
//
// 只复制主库文件是不够的：WAL 模式下最新已提交的数据可能还在 .db-wal 里，主库
// 本身是旧快照。TRUNCATE checkpoint 把 WAL 折回主库并清空，之后复制到的才是
// 完整数据。若 checkpoint 因别的连接占着 WAL 而失败，我们宁可放弃这次备份 ——
// 一个缺数据的备份比没有备份更危险，因为用户会以为可以撤销。
func (c *Cleaner) backupSQLiteFile(dbPath, batchID string, namer *backupNamer) (string, error) {
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, false))
	if err != nil {
		return "", err
	}
	db.SetMaxOpenConns(1)
	var busy, logFrames, checkpointed int
	err = db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed)
	db.Close()
	if err != nil {
		return "", fmt.Errorf("checkpoint 失败: %w", err)
	}
	if busy != 0 {
		return "", fmt.Errorf("WAL 仍被其它连接占用（busy=%d）", busy)
	}
	return c.backupFile(dbPath, batchID, namer)
}

// deleteSQLiteStore 在一个事务里删主表与 Rows 声明的子表。
//
// 子表用 `table:fkcol` 声明而不是硬编码：不同 Agent 的孤儿数据分布不一样，
// 但「主行 ID 出现在子表某个外键列」是共同模式。
func deleteSQLiteStore(db *sql.DB, store catalog.Store, ids []string) ([]journalRow, error) {
	if store.Table == "" || store.IDCol == "" {
		return nil, fmt.Errorf("SQLite 存储缺少 table 或 idCol 声明")
	}
	if len(ids) == 0 {
		return nil, nil
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	rows, err := deleteInTx(tx, store, ids)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return rows, nil
}

func deleteInTx(tx *sql.Tx, store catalog.Store, ids []string) ([]journalRow, error) {
	rows := make([]journalRow, 0, 1+len(store.Rows))
	if err := execDelete(tx, store.Table, store.IDCol, ids); err != nil {
		return nil, fmt.Errorf("删主表 %s 失败: %w", store.Table, err)
	}
	rows = append(rows, journalRow{Table: store.Table, IDs: append([]string(nil), ids...)})

	for _, declaration := range store.Rows {
		table, fk, ok := strings.Cut(declaration, ":")
		if !ok || table == "" || fk == "" {
			// 声明写坏了就跳过这一条，而不是猜一个列名去删 —— 猜错列的 DELETE
			// 会以「删了 0 行」静默成功，用户永远不知道孤儿数据还在。
			continue
		}
		if err := execDelete(tx, table, fk, ids); err != nil {
			return nil, fmt.Errorf("删子表 %s.%s 失败: %w", table, fk, err)
		}
		rows = append(rows, journalRow{Table: table, IDs: append([]string(nil), ids...)})
	}
	return rows, nil
}

// execDelete 分段执行 IN 删除。
//
// SQLite 的变量上限随版本变化（常见 999 / 32766）。一次删几千条会话时把全部
// ID 塞进一条 SQL 会在编译期报 too many SQL variables；按 400 分段可以覆盖
// 任何现代版本，又不需要引入 driver 私有的限制常量。
//
// 删之前先校验 target 真实存在（validateDeleteTarget）：DQS 会把写错的
// "column" 当字符串字面量，SQL 本身不会报错。
func execDelete(tx *sql.Tx, table, column string, ids []string) error {
	if err := validateDeleteTarget(tx, table, column); err != nil {
		return err
	}
	const chunk = 400
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")
		query := "DELETE FROM " + quoteIdent(table) +
			" WHERE " + quoteIdent(column) + " IN (" + placeholders + ")"
		args := make([]any, len(part))
		for i, id := range part {
			args[i] = id
		}
		if _, err := tx.Exec(query, args...); err != nil {
			return err
		}
	}
	return nil
}

// validateDeleteTarget 确认 table 存在，且 column 真的是这张表的一列。
//
// 为什么必须显式校验：SQLite 默认开启「双引号字符串」兼容行为（DQS），
// `DELETE FROM "t" WHERE "写错的列" IN (...)` 里的双引号标识符解析不到列时
// 会被降级成字符串字面量，谓词恒假 —— 语句影响 0 行、不报错、事务照常提交。
// 后果是主表删了、子表/索引没删，journal 里还记着一条指向不存在列的删除，
// 撤销时无从恢复。这种静默失败比直接报错难查得多，所以在拼 SQL 之前把
// 表名与列名对着 PRAGMA table_info 验一遍。
//
// PRAGMA 不支持参数占位，表名只能拼进语句；它来自 catalog 声明，按既有约定
// 过 quoteIdent（与 execDelete 里的拼法一致）。
func validateDeleteTarget(tx *sql.Tx, table, column string) error {
	rows, err := tx.Query("PRAGMA table_info(" + quoteIdent(table) + ")")
	if err != nil {
		return fmt.Errorf("读取表 %s 的结构失败: %w", table, err)
	}
	defer rows.Close()

	foundColumn := false
	hasTable := false
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, ctype      string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("读取表 %s 的列信息失败: %w", table, err)
		}
		hasTable = true
		if name == column {
			foundColumn = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取表 %s 的列信息失败: %w", table, err)
	}
	// PRAGMA table_info 对不存在的表返回 0 行而不是报错，需要单独区分。
	if !hasTable {
		return fmt.Errorf("表 %s 不存在", table)
	}
	if !foundColumn {
		return fmt.Errorf("表 %s 没有列 %s：声明写错会让 DELETE 静默影响 0 行", table, column)
	}
	return nil
}

// trimSQLiteIndex 处理 Kind=="sqlite-rows" 的索引库。
//
// 索引库只删一张表，没有子表声明。备份失败就整段跳过 —— 索引库通常也是 Agent
// 唯一的搜索状态，破坏它比留下几条空索引更难修。
func (c *Cleaner) trimSQLiteIndex(
	idx catalog.Index,
	dbPath string,
	deleted map[string]bool,
	j *journal,
	res *Result,
	namer *backupNamer,
) {
	if !fsx.Exists(dbPath) {
		return
	}
	ids := sortedIDs(deleted)
	if len(ids) == 0 {
		return
	}
	backup, err := c.backupSQLiteFile(dbPath, j.BatchID, namer)
	if err != nil {
		res.Errors = append(res.Errors, ItemError{
			Message: fmt.Sprintf("备份索引库 %s 失败，已跳过: %v", dbPath, err),
		})
		return
	}

	// 与 deleteSQLiteSessions 同一口径：在动库之前记录「主库 + WAL + SHM」的
	// 足迹，VACUUM 并关库之后再算差；只看主库会把 WAL 的变化算漏。
	before := sqliteFootprint(dbPath)

	// 先记账、后裁剪：预写「这个库的这张表将删这些 ID」并落盘，再动库。
	planned := []journalRow{{Table: idx.Table, IDs: append([]string(nil), ids...)}}
	j.SQLite = append(j.SQLite, journalDB{DB: dbPath, Backup: backup, Deletes: planned})
	if err := j.save(); err != nil {
		popLastJournalDB(j)
		j.persistErr = err
		return
	}
	journalBeforeActionHook()

	db, err := sql.Open("sqlite", sqliteDSN(dbPath, false))
	if err != nil {
		popLastJournalDB(j)
		if saveErr := j.save(); saveErr != nil {
			j.persistErr = saveErr
			return
		}
		res.Errors = append(res.Errors, ItemError{
			Message: fmt.Sprintf("打开索引库 %s 失败: %v", dbPath, err),
		})
		return
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		popLastJournalDB(j)
		if saveErr := j.save(); saveErr != nil {
			j.persistErr = saveErr
			return
		}
		res.Errors = append(res.Errors, ItemError{
			Message: fmt.Sprintf("打开索引库 %s 失败: %v", dbPath, err),
		})
		return
	}

	store := catalog.Store{Kind: "sqlite", Table: idx.Table, IDCol: idx.IDCol}
	rows, err := deleteSQLiteStore(db, store, ids)
	if err != nil {
		db.Close()
		popLastJournalDB(j)
		if saveErr := j.save(); saveErr != nil {
			j.persistErr = saveErr
			return
		}
		res.Errors = append(res.Errors, ItemError{
			Message: fmt.Sprintf("裁剪索引库 %s 失败: %v", dbPath, err),
		})
		return
	}
	// 用实际删掉的表/ID 替换预写值（正常情况一致；不一致说明声明与实际不符）。
	j.SQLite[len(j.SQLite)-1].Deletes = rows
	if err := j.save(); err != nil {
		db.Close()
		j.persistErr = err
		return
	}

	vacuumErr := error(nil)
	if _, err := db.Exec("VACUUM"); err != nil {
		vacuumErr = err
	}
	db.Close()
	if freed := freedFromFootprint(before, sqliteFootprint(dbPath)); freed > 0 {
		res.Freed += freed
	}
	if vacuumErr != nil {
		res.Errors = append(res.Errors, ItemError{
			Message: fmt.Sprintf("VACUUM 索引库 %s 失败: %v", dbPath, vacuumErr),
		})
	}
}

// quoteIdent 转义 SQLite 标识符。
//
// 表名/列名来自 spec（可能由用户自定义适配器提供），不能直接拼字符串；
// 双引号内部再双写一个双引号是 SQL 标准的转义方式。
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// plannedJournalRows 在删行之前把 Store 声明翻译成「将要删哪些表、哪些 ID」。
//
// 撤销靠整库备份，Deletes 只是账本信息；提前写下来才能在崩溃窗口里回答
// 「这个库本来要删什么」。Rows 声明是 `table:fkcol`，这里只取表名。
func plannedJournalRows(store catalog.Store, ids []string) []journalRow {
	if store.Table == "" || len(ids) == 0 {
		return nil
	}
	rows := []journalRow{{Table: store.Table, IDs: append([]string(nil), ids...)}}
	for _, declaration := range store.Rows {
		table, _, ok := strings.Cut(declaration, ":")
		if !ok || table == "" {
			continue
		}
		rows = append(rows, journalRow{Table: table, IDs: append([]string(nil), ids...)})
	}
	return rows
}

// plannedPlanRows 汇总一条 plan 里所有 Store 的「计划删除」。
func plannedPlanRows(plan *sqlitePlan) []journalRow {
	var rows []journalRow
	for _, store := range plan.stores {
		rows = append(rows, plannedJournalRows(store, plan.ids)...)
	}
	return rows
}

// sqliteFootprint 返回库文件连同 -wal/-shm 的字节数之和。
//
// 只看主库会把两种真实变化算漏：删行先落进 WAL（主库没缩、WAL 涨了），
// 或 VACUUM 缩了主库但 WAL 还占着空间。文件不存在按 0 计。
func sqliteFootprint(path string) int64 {
	var total int64
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Lstat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		total += info.Size()
	}
	return total
}

// freedFromFootprint 把「动作前后的足迹」换成可上报的释放量。
//
// 只报正数：并发写入可能让 after > before，负的「释放了 -12KB」对用户没有
// 意义，也容易让 UI 出现诡异常数。0 与正数直接返回。
func freedFromFootprint(before, after int64) int64 {
	if before <= after {
		return 0
	}
	return before - after
}
