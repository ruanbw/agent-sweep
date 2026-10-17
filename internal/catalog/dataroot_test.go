package catalog

import (
	"os"
	"strings"
	"testing"
)

// ValidateDataRoot 是删除/扫描共用的护栏：数据根来自环境变量时可以直接是
// / 或用户主目录，包含性检查会因此形同虚设。这里逐条钉住规则。
func TestValidateDataRootRejectsFilesystemRoot(t *testing.T) {
	err := ValidateDataRoot("/")
	if err == nil {
		t.Fatal("/ 不能作为数据根，应当报错")
	}
	if !strings.Contains(err.Error(), "/") {
		t.Errorf("错误信息应带上具体路径，实际为：%v", err)
	}
}

func TestValidateDataRootRejectsUserHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	err := ValidateDataRoot(home)
	if err == nil {
		t.Fatal("用户主目录本身不能作为数据根，应当报错")
	}
	if !strings.Contains(err.Error(), home) {
		t.Errorf("错误信息应带上具体路径 %q，实际为：%v", home, err)
	}

	// 结尾多一条斜杠不能让校验漏判：Clean 之后仍是同一个根。
	if err := ValidateDataRoot(home + string(os.PathSeparator)); err == nil {
		t.Errorf("带尾斜杠的主目录 %q/ 也应被拒绝", home)
	}
}

func TestValidateDataRootRejectsRelativePath(t *testing.T) {
	err := ValidateDataRoot("relative/path")
	if err == nil {
		t.Fatal("相对路径不能作为数据根，应当报错")
	}
	if !strings.Contains(err.Error(), "relative/path") {
		t.Errorf("错误信息应带上具体路径，实际为：%v", err)
	}
}

func TestValidateDataRootRejectsEmpty(t *testing.T) {
	if err := ValidateDataRoot(""); err == nil {
		t.Fatal("空串不能作为数据根，应当报错")
	}
}

// 正常声明不能误杀：临时目录是合法的数据根（存在性不在这里判断）。
func TestValidateDataRootAcceptsTempDir(t *testing.T) {
	if err := ValidateDataRoot(t.TempDir()); err != nil {
		t.Fatalf("临时目录应当是可信数据根，got err: %v", err)
	}
}
