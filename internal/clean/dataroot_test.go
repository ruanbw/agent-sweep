package clean

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/fsx"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// TestDeleteRejectsUntrustedDataRoot 回归审计 D3：数据根来自环境变量，被误配成
// / 或用户主目录时，「路径必须落在根内」的护栏等于空转。Delete 必须在取锁、
// 预留批次之前就拒绝，否则会留下可撤销的假账本、甚至真的动盘。
func TestDeleteRejectsUntrustedDataRoot(t *testing.T) {
	f := newFixture(t, ModePermanent) // HOME 已隔离到 t.TempDir()，不会碰真实主目录
	spec := catalog.Spec{ID: "demo"}
	sessions := []session.Session{{
		ID:      "s1",
		UID:     "demo:s1",
		AgentID: "demo",
		Storage: session.StorageSQLite,
	}}

	cases := []struct {
		name string
		home string
	}{
		{"文件系统根", string(filepath.Separator)},
		{"用户主目录", f.userHome},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := f.cleaner.Delete(spec, tc.home, sessions)
			if err == nil {
				t.Fatalf("Delete(home=%q) 应拒绝不可信的数据根", tc.home)
			}
			if !strings.Contains(err.Error(), "数据根") {
				t.Errorf("错误信息应说明数据根不可信，实际为：%v", err)
			}
			// 拒删必须发生在「预留批次 / 建目录」之前：没有 batchID，也没有
			// 任何 journal/备份目录落地，用户不会看到一份假的可撤销记录。
			if res.BatchID != "" {
				t.Errorf("拒删不该预留批次，BatchID = %q", res.BatchID)
			}
			if fsx.Exists(f.journalDir) {
				t.Errorf("拒删仍创建了 journal 目录: %s", f.journalDir)
			}
			if fsx.Exists(f.backupDir) {
				t.Errorf("拒删仍创建了备份目录: %s", f.backupDir)
			}
		})
	}
}
