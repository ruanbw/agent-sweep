package scan

import (
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
)

// TestOneRejectsUntrustedDataRoot 回归审计 D3 的扫描侧：Env 命中就直接采信变量
// 值，CLAUDE_CONFIG_DIR=/ 之类会让扫描去遍历整棵树。不可信的根必须只报错、
// 不遍历，并且不能标成「已安装」。
func TestOneRejectsUntrustedDataRoot(t *testing.T) {
	result := One(catalog.Spec{ID: "evil", Name: "Evil", Roots: []string{"/"}})

	if result.Err == "" {
		t.Fatal("数据根是 / 时 One 必须报错，而不是去遍历整棵树")
	}
	if result.Installed {
		t.Error("不可信的数据根不该标成已安装")
	}
	if len(result.Sessions) != 0 {
		t.Errorf("不可信的数据根不该产出会话，got %d 条", len(result.Sessions))
	}
}

// 正常根不能被误杀：临时目录存在、没有 Store 时应当是「已安装但 0 条」。
func TestOneAcceptsNormalDataRoot(t *testing.T) {
	home := t.TempDir()
	result := One(catalog.Spec{ID: "ok", Name: "OK", Roots: []string{home}})

	if result.Err != "" {
		t.Fatalf("正常数据根不该报错，got: %s", result.Err)
	}
	if !result.Installed {
		t.Error("存在的正常数据根应当标成已安装")
	}
	if len(result.Sessions) != 0 {
		t.Errorf("没有 Store 时不该产出会话，got %d 条", len(result.Sessions))
	}
}
