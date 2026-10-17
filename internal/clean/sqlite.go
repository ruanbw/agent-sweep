package clean

import (
	"database/sql"
	"fmt"
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
	for _, store := range spec.Stores {
		if store.Kind != "sqlite" {
			continue
		}
		path, err := resolveInHome(home, store.DB)
		if err != nil {
			for _, s := range sessions {
				res.Errors = append(res.Errors, ItemError{
					UID:     s.UID,
					Message: fmt.Sprintf("SQLite 库路径不安全，已跳过: %v", err),
				})
			}
			return
		}
		plan := byPath[path]
		if plan == nil {
			plan = &sqlitePlan{path: path}
			byPath[path] = plan
			plans = append(plans, plan)
		}
		plan.stores = append(plan.stores, store)
	}
	if len(plans) == 0 {
		for _, s := range sessions {
			res.Errors = append(res.Errors, ItemError{
				UID:     s.UID,
				Message: "spec 没有声明可用的 SQLite 存储，无法删除该会话",
			})
		}
		return
	}

	ids := make([]string, 0, len(sessions))
	seen := map[string]bool{}
	for _, s := range sessions {
		if s.ID == "" || seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		ids = append(ids, s.ID)
	}
	if len(ids) == 0 {
		// 一个可用的 ID 都没有时不要碰库：DELETE 空集合 + VACUUM 仍然会改文件，
		// 既拿不到任何删除效果，又多了一份需要撤销的写入。
		for _, s := range sessions {
			res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: "会话缺少 ID，无法定位 SQLite 行"})
		}
		return
	}

	for _, plan := range plans {
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
		if plan.backupErr != nil {
			continue
		}
		db, err := sql.Open("sqlite", sqliteDSN(plan.path, false))
		if err != nil {
			plan.deleteErr = err
			continue
		}
		db.SetMaxOpenConns(1)
		if err := db.Ping(); err != nil {
			plan.deleteErr = err
			db.Close()
			continue
		}

		for _, store := range plan.stores {
			rows, err := deleteSQLiteStore(db, store, ids)
			if err != nil {
				plan.deleteErr = err
				break
			}
			plan.rows = append(plan.rows, rows...)
		}
		// 已提交的删除必须在继续之前落进 journal：多表 spec 里第二张表失败时，
		// 第一张表的行已经真没了，不能因为整条计划被判失败就让撤销对它失明。
		if len(plan.rows) > 0 {
			j.SQLite = append(j.SQLite, journalDB{DB: plan.path, Backup: plan.backup, Deletes: plan.rows})
		}
		if plan.deleteErr != nil {
			db.Close()
			continue
		}

		before := fileSize(plan.path)
		if _, err := db.Exec("VACUUM"); err != nil {
			plan.vacuumErr = err
		}
		db.Close()
		if freed := before - fileSize(plan.path); freed > 0 {
			res.Freed += freed
		}
	}

	for _, s := range sessions {
		if s.ID == "" {
			res.Errors = append(res.Errors, ItemError{UID: s.UID, Message: "会话缺少 ID，无法定位 SQLite 行"})
			continue
		}
		failed := false
		for _, plan := range plans {
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
		c.deleteSidecars(s, home, trashRoot, trashErr, j, res)
		res.Count++
		addDeletedID(deleted, s)
		addAgent(agents, s, spec)
	}
}

// sqliteDSN 统一 DSN 形态。
//
// busy_timeout 不是可选项：Agent 自己往往正开着库（Zed、VS Code 都是常驻进程），
// 没有等待就会随机报 database is locked，用户看到的是「有时能删有时不能」。
// mode=ro 只给只读场景用；删除必须可写，绝不能顺手带上。
func sqliteDSN(path string, readonly bool) string {
	dsn := "file:" + path + "?_pragma=busy_timeout(2000)"
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
func execDelete(tx *sql.Tx, table, column string, ids []string) error {
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

	db, err := sql.Open("sqlite", sqliteDSN(dbPath, false))
	if err != nil {
		res.Errors = append(res.Errors, ItemError{
			Message: fmt.Sprintf("打开索引库 %s 失败: %v", dbPath, err),
		})
		return
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		res.Errors = append(res.Errors, ItemError{
			Message: fmt.Sprintf("打开索引库 %s 失败: %v", dbPath, err),
		})
		return
	}

	store := catalog.Store{Kind: "sqlite", Table: idx.Table, IDCol: idx.IDCol}
	rows, err := deleteSQLiteStore(db, store, ids)
	if err != nil {
		db.Close()
		res.Errors = append(res.Errors, ItemError{
			Message: fmt.Sprintf("裁剪索引库 %s 失败: %v", dbPath, err),
		})
		return
	}

	before := fileSize(dbPath)
	vacuumErr := error(nil)
	if _, err := db.Exec("VACUUM"); err != nil {
		vacuumErr = err
	}
	db.Close()
	if freed := before - fileSize(dbPath); freed > 0 {
		res.Freed += freed
	}
	if vacuumErr != nil {
		res.Errors = append(res.Errors, ItemError{
			Message: fmt.Sprintf("VACUUM 索引库 %s 失败: %v", dbPath, vacuumErr),
		})
	}
	j.SQLite = append(j.SQLite, journalDB{DB: dbPath, Backup: backup, Deletes: rows})
}

// quoteIdent 转义 SQLite 标识符。
//
// 表名/列名来自 spec（可能由用户自定义适配器提供），不能直接拼字符串；
// 双引号内部再双写一个双引号是 SQL 标准的转义方式。
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
