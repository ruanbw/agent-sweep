// 本文件只做一件事：判断一个 .db/.sqlite 文件里有没有「会话表」。
//
// 为什么值得为它单开一个文件：SQLite 打不开、被锁、或者压根不是数据库时，
// 上层（真正的扫描引擎）需要的是「跳过」，而不是一个 panic 或一次几十秒的等待。
// 所以这里同时做了三层保护：mode=ro、busy_timeout、query_only，以及 per-query
// 的 context 超时；最后再用 recover 兜住驱动可能抛出的意外 panic。
package probe

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// sqliteHit 是一条「这个库像会话存储」的证据。rows 是读到的行数，
// 也是 SessionCount 的来源；行数超过上限时只报上限，避免为计数扫全表。
type sqliteHit struct {
	path          string
	table         string
	idCol         string
	titleCol      string
	timeCol       string
	cwdCol        string
	rows          int
	rowsTruncated bool
	quality       int // 组内比较用：表名越像会话表、列越全，分数越高
}

func (h *sqliteHit) evidence() string {
	text := fmt.Sprintf("SQLite 库 %s 中存在会话表 %q", filepath.Base(h.path), h.table)
	if h.idCol != "" {
		text += "，主键 " + h.idCol
	}
	if h.rowsTruncated {
		text += fmt.Sprintf("，行数超过 %d 已截断", sessionRowLimit)
	}
	return text
}

// probeSQLite 打开一个候选库并找会话表。
//
// 打不开（不是 SQLite、权限不足、写锁冲突、超时）一律返回 nil —— 发现流程里
// 「没证据」和「读不了」的结论是一样的，都不该让整次扫描失败。
func probeSQLite(path string, deadline time.Time) (hit *sqliteHit) {
	// 驱动（或损坏文件）理论上可能 panic；发现页不能因为一个坏文件整页空白。
	defer func() {
		if recover() != nil {
			hit = nil
		}
	}()

	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return nil
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nil
	}
	// 单库 400ms 足够读 sqlite_master；真正慢的是被别的进程独占锁住的库。
	timeout := 400 * time.Millisecond
	if remaining < timeout {
		timeout = remaining
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	db, err := sql.Open("sqlite", sqliteReadOnlyDSN(path))
	if err != nil {
		return nil
	}
	defer db.Close()
	// 所有 PRAGMA 都是连接级的。只留一个连接，保证 busy_timeout/query_only
	// 和后面的查询落在同一个连接上。
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout = 500"); err != nil {
		return nil
	}
	// query_only 是最后一道保险：即便 DSN 的 mode=ro 因为某些驱动/文件系统
	// 没生效，也不会有人往用户库里写东西。
	if _, err := db.ExecContext(ctx, "PRAGMA query_only = 1"); err != nil {
		return nil
	}

	table, tableScore := pickSessionTable(ctx, db)
	if table == "" {
		return nil
	}

	hit = &sqliteHit{path: path, table: table, quality: tableScore}
	columns, err := tableColumns(ctx, db, table)
	if err != nil {
		// 读不到列信息就无法确定主键与展示列，草稿会变成一个「删不动的适配器」。
		// 发现流程宁可漏报，也不能让用户保存一个用不了的声明 —— 这里直接放弃
		// 这个候选，不产生半成品。
		return nil
	}
	if id := firstPrimaryKey(columns); id != "" {
		hit.idCol = id
		hit.quality += 8 // 有主键才能安全删行，权重给得比展示列高
	} else if id := firstNamedColumn(columns, idCandidates); id != "" {
		// 不少工具用隐式 rowid，没有声明主键；退而求其次找常见 id 列。
		hit.idCol = id
		hit.quality += 3
	}
	if title := firstNamedColumn(columns, titleCandidates); title != "" {
		hit.titleCol = title
		hit.quality += 2
	}
	if timeCol := firstNamedColumn(columns, timeCandidates); timeCol != "" {
		hit.timeCol = timeCol
		hit.quality += 2
	}
	if cwd := firstNamedColumn(columns, cwdCandidates); cwd != "" {
		hit.cwdCol = cwd
		hit.quality++
	}

	// 行数用 LIMIT 子查询数：大表也只扫到 5001 行，够判断「有会话」了。
	// 失败（比如表结构特殊/超时）就保留 0，由下面的可用性判定把候选拒掉。
	countSQL := fmt.Sprintf("SELECT COUNT(*) FROM (SELECT 1 FROM %s LIMIT %d)",
		quoteIdentifier(table), sessionRowLimit+1)
	var count int
	if err := db.QueryRowContext(ctx, countSQL).Scan(&count); err == nil {
		if count > sessionRowLimit {
			count = sessionRowLimit
			hit.rowsTruncated = true
		}
		hit.rows = count
		if count > 0 {
			hit.quality += 2
		}
	}

	// 三个条件同时满足才算「可用的会话表」，任一不满足就放弃这个候选：
	//
	//  1. 能确定 id 列（显式主键，或 idCandidates 命中）。没有它就无法定位
	//     要删的行，草稿存下去也是个删不动的适配器。
	//  2. 至少有一个展示/时间/路径列。只有 id 的表连标题都显示不出来，多半
	//     不是会话存储 —— 普通应用的 messages(id, body) 正是典型。
	//  3. rows > 0。空表没有任何会话，选它只会往发现页塞一条噪声候选；行数
	//     查询失败也归到这里（无法证明有会话，就不要推荐）。
	//
	// 收紧的代价是可能少几个误报候选，收益是「发现的适配器保存后真的能用」。
	if hit.idCol == "" {
		return nil
	}
	if hit.titleCol == "" && hit.timeCol == "" && hit.cwdCol == "" {
		return nil
	}
	if hit.rows == 0 {
		return nil
	}

	return hit
}

// sqliteReadOnlyDSN 拼出 `file:<path>?mode=ro&_pragma=...`。
//
// 路径只做 EscapedPath（空格 → %20，`?`/`#` 也转义），不把整条 URL 交给
// url.URL.String()：后者会生成 file:///... 三斜杠形式，虽然 SQLite 也认，
// 但和驱动文档里的 `file:demo.db?...` 形态不一致，排查问题时容易误判。
func sqliteReadOnlyDSN(path string) string {
	escaped := (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()
	// modernc 驱动按顺序应用 _pragma；busy_timeout 放前面，遇到短暂锁时等待
	// 而不是立刻报 "database is locked"。
	return "file:" + escaped + "?mode=ro&_pragma=busy_timeout(500)&_pragma=query_only(1)"
}

// pickSessionTable 从 sqlite_master 里挑最像会话表的那个。
// 「包含 session/thread/conversation/message/chat」是任务给的硬条件；
// 在此基础上优先精确名（session、threads…），并压低 FTS/索引表，
// 否则 `messages_fts` 可能抢在真表 `session` 前面被选中。
func pickSessionTable(ctx context.Context, db *sql.DB) (string, int) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
	if err != nil {
		return "", 0
	}
	defer rows.Close()

	best, bestScore := "", 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		score := sessionTableScore(name)
		if score > bestScore || (score == bestScore && score > 0 && name < best) {
			best, bestScore = name, score
		}
	}
	if rows.Err() != nil {
		return "", 0
	}
	return best, bestScore
}

func sessionTableScore(name string) int {
	lower := strings.ToLower(name)
	keywords := []string{"session", "thread", "conversation", "message", "chat"}
	score := 0
	for _, keyword := range keywords {
		switch {
		case lower == keyword || lower == keyword+"s":
			score = 60
		case strings.HasPrefix(lower, keyword):
			score = maxInt(score, 45)
		case strings.Contains(lower, keyword):
			score = maxInt(score, 20)
		}
	}
	if score == 0 {
		return 0
	}
	// FTS 影子表/索引表常带会话词但正文不在里面；降权但不清零，
	// 因为某些 Agent 只把会话存在 FTS 表里。
	if strings.Contains(lower, "fts") {
		score -= 25
	}
	if strings.HasPrefix(lower, "sqlite_") {
		score -= 40
	}
	return maxInt(score, 1)
}

type sqliteColumn struct {
	name string
	pk   int
}

// tableColumns 读列信息。pragma_table_info 是表值函数，可以用参数绑定，
// 避免把 sqlite_master 里读到的表名拼进 SQL（虽然那里读到的名字本身可信，
// 但少一次字符串拼接就少一类注入面）。
func tableColumns(ctx context.Context, db *sql.DB, table string) ([]sqliteColumn, error) {
	rows, err := db.QueryContext(ctx, "SELECT name, pk FROM pragma_table_info(?) ORDER BY cid", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var columns []sqliteColumn
	for rows.Next() {
		var column sqliteColumn
		if err := rows.Scan(&column.name, &column.pk); err != nil {
			continue
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

var idCandidates = []string{"id", "session_id", "thread_id", "conversation_id", "chat_id", "uuid"}
var titleCandidates = []string{"title", "name", "summary", "subject", "label", "display_name", "thread_name"}
var timeCandidates = []string{
	"updated_at", "time_updated", "last_modified_time", "modified_at",
	"last_updated", "created_at", "timestamp", "time", "mtime", "date",
}
var cwdCandidates = []string{
	"cwd", "directory", "workspace", "workspace_uris",
	"folder_paths", "project_path", "project", "path",
}

func firstPrimaryKey(columns []sqliteColumn) string {
	for _, column := range columns {
		if column.pk > 0 {
			return column.name
		}
	}
	return ""
}

func firstNamedColumn(columns []sqliteColumn, candidates []string) string {
	for _, candidate := range candidates {
		for _, column := range columns {
			if strings.EqualFold(column.name, candidate) {
				return column.name
			}
		}
	}
	return ""
}

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
