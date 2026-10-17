// 本文件处理「宿主数据根里的跨 Agent 索引」：最典型的是 VS Code 的
// globalStorage/agent-host.db，Claude Code / Copilot CLI 的会话登记在里面。
//
// 它与普通 Index 的区别只有两点：库在**另一个** Spec 的 Home 下；ID 列存的是
// URI 而不是裸 ID。除此之外的纪律完全一样：先备份整库、再删行、最后 VACUUM，
// 每一步失败都只记账，不影响同批次的正文删除。
package clean

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/fsx"
)

// sharedHostHome 解析共享索引宿主的数据根。
//
// 抽成包级变量只为测试能指向临时目录；生产路径就是 catalog.Find(host).Home(nil)。
// 包内用例都不并行执行，替换后必须立刻复原。
var sharedHostHome = func(hostID string) string {
	spec, ok := catalog.Find(hostID)
	if !ok {
		return ""
	}
	return spec.Home(nil)
}

// trimSharedIndexes 裁剪本批次涉及 Agent 的共享索引。
//
// 调用时机与 trimIndexes 相同：deleted 已经生成。匹配不到任何行是正常情况
// （宿主注册表可能还没回填），此时既不报错也不写 journal、不做备份。
func (c *Cleaner) trimSharedIndexes(
	spec catalog.Spec,
	deleted map[string]bool,
	namer *backupNamer,
	j *journal,
	res *Result,
) {
	ids := sortedIDs(deleted)
	if len(ids) == 0 {
		return
	}
	for _, shared := range catalog.SharedIndexes() {
		if !containsText(shared.Agents, spec.ID) {
			continue
		}
		hostHome := sharedHostHome(shared.Host)
		if hostHome == "" {
			// 宿主没装（或数据根不存在）：没有可裁的东西，静默跳过。
			continue
		}
		dbPath, err := resolveInHome(hostHome, shared.DB)
		if err != nil {
			res.Errors = append(res.Errors, ItemError{
				Message: fmt.Sprintf("共享索引 %s 的数据库路径不安全，已跳过: %v", shared.DB, err),
			})
			continue
		}
		if !fsx.Exists(dbPath) {
			continue
		}
		c.trimSharedIndexDB(shared, dbPath, ids, namer, j, res)
	}
}

// trimSharedIndexDB 处理一个共享索引库：先找出命中的行值，再备份、逐表删除。
//
// 先 SELECT 后 DELETE 而不是直接 DELETE ... LIKE：journal 需要记下真正被删的
// URI（撤销审计用），而 DELETE 的 RowsAffected 拿不到这些值；另外查出命中集后
// 删除可以走精确 IN，不必让 LIKE 参与写路径。
func (c *Cleaner) trimSharedIndexDB(
	shared catalog.SharedIndex,
	dbPath string,
	ids []string,
	namer *backupNamer,
	j *journal,
	res *Result,
) {
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, false))
	if err != nil {
		res.Errors = append(res.Errors, ItemError{
			Message: fmt.Sprintf("打开共享索引 %s 失败: %v", dbPath, err),
		})
		return
	}
	db.SetMaxOpenConns(1)

	tables, err := sharedIndexTables(db, shared)
	if err != nil {
		db.Close()
		res.Errors = append(res.Errors, ItemError{
			Message: fmt.Sprintf("读取共享索引 %s 的表结构失败: %v", dbPath, err),
		})
		return
	}

	matched := map[string][]string{}
	total := 0
	for _, table := range tables {
		values, err := matchSessionURIs(db, table, shared.IDCol, ids)
		if err != nil {
			res.Errors = append(res.Errors, ItemError{
				Message: fmt.Sprintf("读取共享索引 %s.%s 失败，已跳过该表: %v", dbPath, table, err),
			})
			continue
		}
		if len(values) == 0 {
			continue
		}
		matched[table] = values
		total += len(values)
	}
	db.Close()
	if total == 0 {
		return
	}

	// 备份放在任何写操作之前：共享索引同样是「改了就只剩备份」的对象。
	backup, err := c.backupSQLiteFile(dbPath, j.BatchID, namer)
	if err != nil {
		res.Errors = append(res.Errors, ItemError{
			Message: fmt.Sprintf("备份共享索引 %s 失败，已跳过裁剪: %v", dbPath, err),
		})
		return
	}

	db, err = sql.Open("sqlite", sqliteDSN(dbPath, false))
	if err != nil {
		res.Errors = append(res.Errors, ItemError{
			Message: fmt.Sprintf("打开共享索引 %s 失败: %v", dbPath, err),
		})
		return
	}
	db.SetMaxOpenConns(1)
	var rows []journalRow
	for _, table := range tables {
		values := matched[table]
		if len(values) == 0 {
			continue
		}
		if err := deleteSharedRows(db, table, shared.IDCol, values); err != nil {
			res.Errors = append(res.Errors, ItemError{
				Message: fmt.Sprintf("裁剪共享索引 %s.%s 失败: %v", dbPath, table, err),
			})
			continue
		}
		rows = append(rows, journalRow{Table: table, IDs: values})
	}
	if len(rows) > 0 {
		j.SQLite = append(j.SQLite, journalDB{DB: dbPath, Backup: backup, Deletes: rows})
	}
	before := fileSize(dbPath)
	if _, err := db.Exec("VACUUM"); err != nil {
		res.Errors = append(res.Errors, ItemError{
			Message: fmt.Sprintf("VACUUM 共享索引 %s 失败: %v", dbPath, err),
		})
	}
	db.Close()
	if freed := before - fileSize(dbPath); freed > 0 {
		res.Freed += freed
	}
}

// sharedIndexTables 返回共享索引里真实存在、且确实有 ID 列的表。
//
// 表/列都可能随宿主版本漂移：缺表或缺列时跳过并提示，而不是让 DELETE 撞上
// 「列不存在」—— 后者在 DQS 下会静默影响 0 行，比报错更难查。
func sharedIndexTables(db *sql.DB, shared catalog.SharedIndex) ([]string, error) {
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]string, 0, len(shared.Tables))
	for _, table := range shared.Tables {
		if !present[table] {
			continue
		}
		has, err := tableHasColumn(db, table, shared.IDCol)
		if err != nil {
			return nil, err
		}
		if has {
			out = append(out, table)
		}
	}
	return out, nil
}

func tableHasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// matchSessionURIs 找出列值与会话 ID 匹配的行值。
//
// 列里存的是 URI（`vscode-chat://claude-code/<id>`），所以除精确相等外还要匹配
// 常见分隔符后的后缀：/、:、#。LIKE 里的通配符按 SQLite 规则转义，避免会话 ID
// 里的 `%`/`_` 意外放宽匹配。
func matchSessionURIs(db *sql.DB, table, column string, ids []string) ([]string, error) {
	const chunk = 400
	seen := map[string]bool{}
	var out []string
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[start:end]
		conditions := make([]string, 0, len(part))
		args := make([]any, 0, len(part)*4)
		quoted := quoteIdent(column)
		for _, id := range part {
			conditions = append(conditions, fmt.Sprintf(
				"(%s = ? OR %s LIKE ? ESCAPE '\\' OR %s LIKE ? ESCAPE '\\' OR %s LIKE ? ESCAPE '\\')",
				quoted, quoted, quoted, quoted))
			escaped := escapeLike(id)
			args = append(args, id, "%/"+escaped, "%:"+escaped, "%#"+escaped)
		}
		query := "SELECT DISTINCT " + quoted + " FROM " + quoteIdent(table) +
			" WHERE " + strings.Join(conditions, " OR ")
		rows, err := db.Query(query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	sort.Strings(out)
	return out, nil
}

// escapeLike 转义 LIKE 的通配符，反斜杠本身也要转义。
func escapeLike(text string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(text)
}

// deleteSharedRows 在一个事务里按精确值删行，沿用主删除路径的 400 分段与
// 表/列真实性校验。
func deleteSharedRows(db *sql.DB, table, column string, values []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := execDelete(tx, table, column, values); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func containsText(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
