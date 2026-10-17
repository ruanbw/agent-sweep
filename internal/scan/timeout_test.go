package scan

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// R4 回归：一个 Agent 的 SQLite 库被它自己占着（Agent 常驻时很常见）时，
// scanSQLite 不能无限等待。两道上限各自都要被真正触发过：
//
//   1. DSN 的 busy_timeout —— 等锁的时长有上限；
//   2. QueryContext 的 sqliteQueryTimeout —— 整条查询的总时长有上限。
//
// 只断言 DSN 字符串是不够的：busy_timeout 写进 query 但没被驱动生效、或者
// QueryContext 的 ctx 被传错成 Background，字符串断言都看不出来。

// seedTimeoutFixture 造一个可扫描的会话库，返回库路径。
//
// 建库阶段用裸路径（不带 file: 前缀）：SQLite 只对 file: 做 URI 解析，
// 裸名按字面路径处理，不会被临时目录里的特殊字符干扰。
func seedTimeoutFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "threads.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("创建夹具库：%v", err)
	}
	if _, err := db.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY, summary TEXT, updated_at INTEGER, folder_paths TEXT)`); err != nil {
		t.Fatalf("建表：%v", err)
	}
	if _, err := db.Exec(`INSERT INTO threads VALUES ('t1', '被锁住的会话', 1792119045599, '/Users/me/proj')`); err != nil {
		t.Fatalf("插行：%v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭夹具库：%v", err)
	}
	return path
}

func timeoutSpec(root string) catalog.Spec {
	return catalog.Spec{
		ID: "zed", Name: "Zed",
		Roots: []string{root},
		Stores: []catalog.Store{{
			Kind: "sqlite", DB: "threads.db", Table: "threads",
			IDCol: "id", TitleCol: "summary", TimeCol: "updated_at", CWDCol: "folder_paths",
		}},
	}
}

// TestSQLiteScanDSNIncludesBusyTimeout 断言扫描用的 DSN 带上 busy_timeout，
// 且只读语义（mode=ro、query_only）一个没丢。
func TestSQLiteScanDSNIncludesBusyTimeout(t *testing.T) {
	dsn := sqliteScanDSN("/tmp/会话 库.db")
	if !strings.Contains(dsn, "busy_timeout") {
		t.Fatalf("扫描 DSN 缺 busy_timeout：%s", dsn)
	}
	// 基础 DSN 被 dsn_test.go 逐字节断言，扫描 DSN 必须是它的超集。
	if base := sqliteDSN("/tmp/会话 库.db"); !strings.HasPrefix(dsn, base+"&") {
		t.Fatalf("扫描 DSN 应建立在 sqliteDSN 之上\n base: %s\n  got: %s", base, dsn)
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("扫描 DSN 不是合法 URL：%v", err)
	}
	query := u.Query()
	if got := query.Get("mode"); got != "ro" {
		t.Errorf("mode = %q，期望 ro", got)
	}
	var busy, queryOnly bool
	for _, pragma := range query["_pragma"] {
		switch {
		case strings.HasPrefix(pragma, "busy_timeout("):
			busy = true
		case pragma == "query_only(1)":
			queryOnly = true
		}
	}
	if !busy {
		t.Errorf("_pragma 里缺 busy_timeout：%v", query["_pragma"])
	}
	if !queryOnly {
		t.Errorf("_pragma 里缺 query_only(1)，只读语义不能丢：%v", query["_pragma"])
	}

	// 取值本身也要守住区间：等锁几百毫秒到两秒，且必须明显短于查询总超时。
	ms := sqliteBusyTimeout / time.Millisecond
	if ms < 200 || ms > 2000 {
		t.Errorf("busy_timeout = %dms，期望 200–2000ms", ms)
	}
	if sqliteBusyTimeout >= sqliteQueryTimeout {
		t.Errorf("busy_timeout(%s) 必须小于查询总超时(%s)：否则报错会退化成含混的「超时」",
			sqliteBusyTimeout, sqliteQueryTimeout)
	}
	if sqliteQueryTimeout < 3*time.Second || sqliteQueryTimeout > 5*time.Second {
		t.Errorf("查询超时 = %s，期望 3–5s", sqliteQueryTimeout)
	}
}

// TestSQLiteScanDSNKeepsReadOnly 证明加了 busy_timeout 之后连接仍然只读。
func TestSQLiteScanDSNKeepsReadOnly(t *testing.T) {
	path := seedTimeoutFixture(t)

	db, err := sql.Open("sqlite", sqliteScanDSN(path))
	if err != nil {
		t.Fatalf("打开夹具库：%v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var queryOnly int
	if err := db.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil {
		t.Fatalf("读 query_only：%v", err)
	}
	if queryOnly != 1 {
		t.Fatalf("PRAGMA query_only = %d，期望 1", queryOnly)
	}
	if _, err := db.Exec("DELETE FROM threads"); err == nil {
		t.Fatal("扫描连接不应允许 DELETE")
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM threads").Scan(&count); err != nil {
		t.Fatalf("计数：%v", err)
	}
	if count != 1 {
		t.Fatalf("行数 = %d，期望 1（删除不应生效）", count)
	}
}

// TestScanSQLiteTimesOutOnLockedDatabase 是 R4 的核心：另一个连接开着长事务
// 占住库时，scanSQLite 必须在有限时间内返回，且 Err 非空。
//
// 为什么用 BEGIN EXCLUSIVE 而不是 BEGIN IMMEDIATE：rollback journal 模式下
// IMMEDIATE 只拿 RESERVED 锁，读事务照样能进（本机实测读 1 行只要 2ms），
// 根本触发不了等锁；EXCLUSIVE 才真正把读也挡住，是「库被独占」的真实形态。
func TestScanSQLiteTimesOutOnLockedDatabase(t *testing.T) {
	path := seedTimeoutFixture(t)

	// 占锁方：单独一个连接，busy_timeout=0，保证它拿到锁就立刻返回。
	holder, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("打开占锁连接：%v", err)
	}
	defer holder.Close()
	holder.SetMaxOpenConns(1)
	holdConn, err := holder.Conn(context.Background())
	if err != nil {
		t.Fatalf("取占锁连接：%v", err)
	}
	defer holdConn.Close()
	if _, err := holdConn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("开长事务：%v", err)
	}
	defer holdConn.ExecContext(context.Background(), "ROLLBACK")

	spec := timeoutSpec(filepath.Dir(path))
	started := time.Now()
	done := make(chan struct {
		items []session.Session
		err   error
	}, 1)
	go func() {
		items, err := scanSQLite(spec, spec.Roots[0], spec.Stores[0])
		done <- struct {
			items []session.Session
			err   error
		}{items, err}
	}()

	// 兜底：上限是 busy_timeout(1.5s)，测试给 15s 余量；再晚就是真挂了。
	// 这里必须用 time.After，否则回归时测试自己会挂到 go test 的全局超时。
	select {
	case got := <-done:
		elapsed := time.Since(started)
		if got.err == nil {
			t.Fatalf("库被独占时应返回错误，实际 nil（读出 %d 条，用时 %s）", len(got.items), elapsed)
		}
		// 允许 items 为空或部分：这里只要求错误不丢。
		if elapsed > 10*time.Second {
			t.Errorf("返回太慢：%s，说明超时上限没生效", elapsed)
		}
		// 没有 busy_timeout 时驱动会立刻报 locked（微秒级）。等到接近上限，
		// 才说明「先等锁、再放弃」这条链路真的接上了。
		if elapsed < 300*time.Millisecond {
			t.Errorf("只用了 %s 就放弃，busy_timeout 似乎没生效", elapsed)
		}
		msg := got.err.Error()
		if !strings.Contains(msg, "threads.db") {
			t.Errorf("错误应带库名（中文上下文），实际 %q", msg)
		}
		if !strings.Contains(msg, "占用") && !strings.Contains(msg, "超时") {
			t.Errorf("错误应说清是被占用或超时，实际 %q", msg)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("scanSQLite 在 15s 内没有返回：R4 回归（库被独占时整次扫描会永久挂住）")
	}
}

// TestOneReportsLockTimeoutAsAgentError 确认错误真的落到 AgentResult.Err ——
// UI 就是从这里读「这个 Agent 超时了」，而不是显示「已安装 · 0 条会话」。
func TestOneReportsLockTimeoutAsAgentError(t *testing.T) {
	path := seedTimeoutFixture(t)

	holder, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("打开占锁连接：%v", err)
	}
	defer holder.Close()
	holder.SetMaxOpenConns(1)
	holdConn, err := holder.Conn(context.Background())
	if err != nil {
		t.Fatalf("取占锁连接：%v", err)
	}
	defer holdConn.Close()
	if _, err := holdConn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("开长事务：%v", err)
	}
	defer holdConn.ExecContext(context.Background(), "ROLLBACK")

	resultCh := make(chan AgentResult, 1)
	go func() { resultCh <- One(timeoutSpec(filepath.Dir(path))) }()

	select {
	case result := <-resultCh:
		if result.Err == "" {
			t.Fatal("AgentResult.Err 为空：UI 会把它当成「0 条会话」")
		}
		if result.Sessions != nil && len(result.Sessions) > 0 {
			// 不禁止部分结果（partial_test.go 保证 items 不丢），只是这里读不到行。
			t.Logf("带回了 %d 条部分结果", len(result.Sessions))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("One 在 15s 内没有返回：R4 回归")
	}
}

// TestScanSQLiteUnlockedDatabaseUnaffected 是反向断言：加超时不能把正常库读坏。
func TestScanSQLiteUnlockedDatabaseUnaffected(t *testing.T) {
	path := seedTimeoutFixture(t)
	spec := timeoutSpec(filepath.Dir(path))

	items, err := scanSQLite(spec, spec.Roots[0], spec.Stores[0])
	if err != nil {
		t.Fatalf("正常库不应报错：%v", err)
	}
	if len(items) != 1 {
		t.Fatalf("期望 1 条会话，实际 %d", len(items))
	}
	if items[0].Title != "被锁住的会话" || items[0].Project != "/Users/me/proj" {
		t.Fatalf("读出来的字段不对：%+v", items[0])
	}
	if !items[0].UpdatedAt.After(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("时间戳没解析出来：%v", items[0].UpdatedAt)
	}
}

// TestSQLiteQueryErrorKeepsCause 单测错误包装：ctx 超时要能被认出来，
// 且中文上下文要带库名 —— 上层（以及将来的调用方）还要 errors.Is 判断。
func TestSQLiteQueryErrorKeepsCause(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, context.Canceled} {
		wrapped := sqliteQueryError("/tmp/agents/threads.db", err)
		if wrapped == nil {
			t.Fatal("非 nil 错误被吞掉了")
		}
		if !strings.Contains(wrapped.Error(), "超时") {
			t.Errorf("错误应说明超时，实际 %q", wrapped)
		}
		if !strings.Contains(wrapped.Error(), "threads.db") {
			t.Errorf("错误应带库名，实际 %q", wrapped)
		}
		if !errors.Is(wrapped, err) {
			t.Errorf("应保留原始错误 %v，实际 %v", err, wrapped)
		}
	}
	if got := sqliteQueryError("/tmp/threads.db", nil); got != nil {
		t.Errorf("nil 错误应原样返回 nil，实际 %v", got)
	}
}
