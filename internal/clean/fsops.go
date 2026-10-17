package clean

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ruanbw/agent-sweep/internal/fsx"
)

// backupNamer 给同一批次里的备份文件编号。
//
// 光靠「扁平化绝对路径」仍可能撞名（例如 `/a/b_c` 和 `/a_b/c` 都会变成 `_a_b_c`），
// 所以每个备份再带一个批次内单调递增的序号。序号放在名字末尾而不是前缀，
// 是为了让同一来源路径的多次备份在目录列表里相邻，排查时好认。
type backupNamer struct {
	seq int
}

func (n *backupNamer) next(absPath string) string {
	n.seq++
	return flattenPath(absPath) + "-" + strconv.Itoa(n.seq)
}

// flattenPath 把绝对路径压成一个文件名。
//
// 保留字母、数字与 `._-`，其余字符一律换成 `_`。不保留路径分隔符是有意的：
// 备份目录要能被 `os.ReadDir` 一层看完，不能变成一棵和原目录同构的树 ——
// 那样「这一批备份了哪些文件」反而要递归才能回答。
func flattenPath(absPath string) string {
	var b strings.Builder
	for _, r := range absPath {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if name == "" {
		name = "file"
	}
	// 单段文件名一般上限 255 字节。超长路径（macOS 的临时目录尤其长）如果不截，
	// 备份会直接失败；截断后加原路径哈希，既保住可读前缀也避免不同路径截成同名。
	if len(name) > 160 {
		sum := sha256.Sum256([]byte(absPath))
		name = name[:140] + "_" + hex.EncodeToString(sum[:8])
	}
	return name
}

// backupFile 把 src 整文件复制进本批次的备份目录。
//
// 备份目录权限 0700、文件名保留原扩展名（扁平化时 . 被保留），是因为这些文件
// 里可能含有用户的会话索引与数据库；它们属于应用私有数据，不该被同机其它用户读。
func (c *Cleaner) backupFile(src, batchID string, namer *backupNamer) (string, error) {
	info, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("备份目标 %s 是目录，不是文件", src)
	}
	dir := filepath.Join(c.opts.BackupDir, batchID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, namer.next(src))
	if err := copyFile(src, dest); err != nil {
		os.Remove(dest)
		return "", err
	}
	return dest, nil
}

// moveToTrash 把一条路径挪进本批次的废纸篓目录。
//
// 真正的移动走 fsx.RenameUnder：从数据根的目录 fd 起逐层 openat(O_NOFOLLOW)
// 下探，再用 renameat 换名，父目录在校验之后被换成符号链接也无法把移动重定向
// 到数据根之外。跨卷（EXDEV）不降级成复制 —— 复制按路径解析，等于把刚闭合的
// TOCTOU 又带回来；这种情况直接报错，让用户改用永久删除或换同卷废纸篓。
//
// 不直接沿用原文件名：同一个 Agent 的多个会话目录里可能都有 `session.json`，
// 平铺到同一个批次目录必然撞名。撞名时加 `-2`、`-3`，而不是覆盖。
func moveToTrash(home, src, trashRoot string) (string, error) {
	if err := os.MkdirAll(trashRoot, 0o700); err != nil {
		return "", err
	}
	if _, err := os.Lstat(src); err != nil {
		return "", fmt.Errorf("源路径不存在或不可访问 %s: %w", src, err)
	}
	base := filepath.Base(filepath.Clean(src))
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "session"
	}
	name := base
	dest := filepath.Join(trashRoot, name)
	for n := 2; pathExists(dest); n++ {
		name = fmt.Sprintf("%s-%d", base, n)
		dest = filepath.Join(trashRoot, name)
	}
	if err := fsx.RenameUnder(home, src, trashRoot, name); err != nil {
		if errors.Is(err, fsx.ErrCrossDevice) {
			return "", fmt.Errorf("移入废纸篓失败：%s 与废纸篓不在同一卷；请改用永久删除，或把废纸篓放到同一卷（%w）", src, err)
		}
		return "", err
	}
	return dest, nil
}

// movePath 先尝试 rename，失败再退回复制 + 删除。
//
// 正常情况（数据根与家目录在同一卷）rename 是原子的，崩溃也不会留下半个文件。
// 但外接盘上的 Agent 数据或测试里的独立临时卷会触发 EXDEV，此时退回复制是唯一
// 让「移入废纸篓」可用的办法；复制成功后才删源，避免中途失败把两边都弄丢。
func movePath(src, dest string) error {
	if err := os.Rename(src, dest); err == nil {
		return nil
	}

	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		if err := copyTree(src, dest); err != nil {
			os.RemoveAll(dest)
			return err
		}
	case info.Mode()&os.ModeSymlink != 0:
		link, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(link, dest); err != nil {
			return err
		}
	default:
		if err := copyFile(src, dest); err != nil {
			os.Remove(dest)
			return err
		}
	}
	return os.RemoveAll(src)
}

// copyTree 递归复制目录，保留权限与符号链接。
//
// 不用 filepath.WalkDir + io.Copy 一把梭，是因为删除路径里可能有指向外部的
// 符号链接；跟随它复制会把废纸篓变成一台「把外部文件吸进来」的机器。
func copyTree(src, dest string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		srcChild := filepath.Join(src, entry.Name())
		destChild := filepath.Join(dest, entry.Name())
		childInfo, err := os.Lstat(srcChild)
		if err != nil {
			return err
		}
		switch {
		case childInfo.IsDir():
			if err := copyTree(srcChild, destChild); err != nil {
				return err
			}
		case childInfo.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(srcChild)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, destChild); err != nil {
				return err
			}
		default:
			if err := copyFile(srcChild, destChild); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyFile 复制单个文件并 fsync。
//
// fsync 是给「备份完成后立刻断电」准备的：不落盘就返回成功，等于告诉上层可以
// 放心删，而实际上备份还在页缓存里。
func copyFile(src, dest string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dest, info.Mode().Perm())
}

// copyFileAtomic 先写临时文件再改名，用于撤销时覆盖原文件。
//
// 撤销发生在「用户已经把东西删了」之后，此时任何半截写入都是二次伤害：
// 索引会被解析成坏 JSON，数据库会直接打不开。临时文件与目标同目录，rename 才是原子的。
func copyFileAtomic(src, dest string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".*.restore")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	in, err := os.Open(src)
	if err != nil {
		tmp.Close()
		return err
	}
	defer in.Close()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// removeSQLiteSidecars 在恢复数据库前清掉 -wal / -shm / -journal。
//
// 主库文件被换回旧快照后，如果旁边还留着新版本产生的 WAL，SQLite 下次打开会
// 把新事务重放到旧库上，轻则数据错乱，重则直接损坏。撤销的正确性优先于
// 「保留删除之后其他人写入的变更」——那些变更本来就在这次撤销的语义之外。
//
// 删除失败必须交给调用方：残留的新 WAL 一旦在下次打开时重放到被覆盖的旧库上，
// 结果就是库损坏，而静默吞错会让这个风险完全不可见。ENOENT 是正常情况
// （库可能本来就没有 sidecar），不算错误；其余失败逐条收集后返回。
func removeSQLiteSidecars(path string) error {
	var errs []error
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("删除 %s 失败: %w", path+suffix, err))
		}
	}
	return errors.Join(errs...)
}
