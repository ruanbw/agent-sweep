// 本文件提供「以数据根为锚的 fd 相对删除/移动」原语。
//
// 为什么需要它们：clean 原来的顺序是 checkPathInHome（词法 + realpath）→
// os.RemoveAll / os.Rename，两步之间 Agent 并发写入可以把中间层目录换成指向
// 数据根之外的符号链接；RemoveAll/Rename 由内核按路径重新解析，就会删/移到根外。
// 这里改成从数据根的目录 fd 出发逐层 openat(O_NOFOLLOW) 下探，最后用
// unlinkat/renameat 操作相对名字 —— 父目录在校验之后被换链也重定向不了删除。
package fsx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// ErrCrossDevice 表示 renameat 跨卷（EXDEV）。
//
// 跨卷时不能退回「复制 + 删除」：那会重新引入路径解析，把刚闭合的 TOCTOU
// 又带回来。调用方应当向用户明确报错（改用永久删除，或把废纸篓放到同一卷）。
var ErrCrossDevice = errors.New("跨卷移动")

// RemoveAllUnder 删除 root 之下的 path。
//
// root 先解析成真实路径并打开目录 fd；path 的每个中间组件都用
// openat(O_NOFOLLOW|O_DIRECTORY) 下探，任意一层是符号链接就直接报错（数据根
// 内部不该有这种结构）。最后一个组件用 Fstatat(AT_SYMLINK_NOFOLLOW) 分类：
// 普通文件与符号链接用 unlinkat 删掉（链接只删链接本身），目录先递归清空再
// unlinkat(AT_REMOVEDIR)。
//
// path 必须在 root 之内（词法检查）；越界、root 本身、中间层符号链接都以错误
// 返回。ENOENT 原样返回，由调用方按「这条删除失败」记账。
func RemoveAllUnder(root, path string) error {
	relative, err := relativeUnder(root, path)
	if err != nil {
		return err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("解析数据根 %s 失败: %w", root, err)
	}
	rootDir, err := openDir(resolvedRoot)
	if err != nil {
		return fmt.Errorf("打开数据根 %s 失败: %w", root, err)
	}
	defer rootDir.Close()

	parts := splitRelative(relative)
	parent, owned, err := descend(rootDir, parts[:len(parts)-1], path)
	if err != nil {
		return err
	}
	if owned {
		defer parent.Close()
	}
	if err := removeEntry(parent, parts[len(parts)-1]); err != nil {
		return fmt.Errorf("删除 %s 失败: %w", path, err)
	}
	return nil
}

// RenameUnder 把 root 之下的 src 移动成 dstDir/dstBase。
//
// src 的中间层同样逐层 openat(O_NOFOLLOW)；目标目录用调用方已经创建好的真实
// 路径（当前是批次废纸篓目录）。目标已存在时直接报错而不覆盖 —— renameat 本身
// 会静默覆盖，所以先 Fstatat 一次。跨卷返回 ErrCrossDevice，绝不退回复制。
func RenameUnder(root, src, dstDir, dstBase string) error {
	relative, err := relativeUnder(root, src)
	if err != nil {
		return err
	}
	if dstBase == "" || dstBase == "." || dstBase == ".." ||
		dstBase != filepath.Base(dstBase) || strings.ContainsAny(dstBase, `/\`) {
		return fmt.Errorf("目标文件名 %q 不合法", dstBase)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("解析数据根 %s 失败: %w", root, err)
	}
	rootDir, err := openDir(resolvedRoot)
	if err != nil {
		return fmt.Errorf("打开数据根 %s 失败: %w", root, err)
	}
	defer rootDir.Close()

	parts := splitRelative(relative)
	parent, owned, err := descend(rootDir, parts[:len(parts)-1], src)
	if err != nil {
		return err
	}
	if owned {
		defer parent.Close()
	}

	dstDirFile, err := openDir(dstDir)
	if err != nil {
		return fmt.Errorf("打开目标目录 %s 失败: %w", dstDir, err)
	}
	defer dstDirFile.Close()

	var stat unix.Stat_t
	switch err := unix.Fstatat(int(dstDirFile.Fd()), dstBase, &stat, unix.AT_SYMLINK_NOFOLLOW); {
	case err == nil:
		return fmt.Errorf("目标已存在: %s", filepath.Join(dstDir, dstBase))
	case !errors.Is(err, unix.ENOENT):
		return fmt.Errorf("检查目标 %s 失败: %w", filepath.Join(dstDir, dstBase), err)
	}

	sourceName := parts[len(parts)-1]
	if err := unix.Renameat(int(parent.Fd()), sourceName, int(dstDirFile.Fd()), dstBase); err != nil {
		if errors.Is(err, unix.EXDEV) {
			return fmt.Errorf("%w：%s → %s", ErrCrossDevice, src, filepath.Join(dstDir, dstBase))
		}
		return fmt.Errorf("移动 %s 失败: %w", src, err)
	}
	return nil
}

// relativeUnder 做词法包含检查并返回 root 内的相对路径。
func relativeUnder(root, path string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("解析数据根 %s 失败: %w", root, err)
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("解析路径 %s 失败: %w", path, err)
	}
	relative, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return "", fmt.Errorf("%s 与数据根 %s 无法比较: %w", path, root, err)
	}
	if relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("%s 不在数据根 %s 内", path, root)
	}
	return relative, nil
}

func splitRelative(relative string) []string {
	return strings.Split(filepath.ToSlash(relative), "/")
}

// openDir 打开调用方给出的真实目录（数据根或已创建的废纸篓目录）。
func openDir(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		unix.Close(fd)
		return nil, fmt.Errorf("无法包装目录 fd: %s", path)
	}
	return file, nil
}

// openDirAt 以父目录 fd 为锚打开子目录，绝不跟随符号链接。
func openDirAt(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		unix.Close(fd)
		return nil, fmt.Errorf("无法包装目录 fd: %s", name)
	}
	return file, nil
}

// descend 从 parent 起逐层打开 components；返回的 owned 表示 dir 是否由本函数
// 新开（需要调用方关闭）。
func descend(parent *os.File, components []string, fullPath string) (dir *os.File, owned bool, err error) {
	dir = parent
	for _, component := range components {
		next, openErr := openDirAt(dir, component)
		if owned {
			dir.Close()
		}
		if openErr != nil {
			return nil, false, fmt.Errorf("打开 %s 的父目录失败（中间层可能是符号链接或不可访问）: %w", fullPath, openErr)
		}
		dir, owned = next, true
	}
	return dir, owned, nil
}

// removeEntry 删除父目录 fd 下的一个名字。
//
// Fstatat 用 AT_SYMLINK_NOFOLLOW：符号链接按「链接本身」处理，不会跟随到目标。
func removeEntry(parent *os.File, name string) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
		return removeDirAt(parent, name)
	}
	return unix.Unlinkat(int(parent.Fd()), name, 0)
}

// removeDirAt 清空并删除一个目录（全部基于 fd 相对操作）。
func removeDirAt(parent *os.File, name string) error {
	dir, err := openDirAt(parent, name)
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		dir.Close()
		return err
	}
	for _, entry := range entries {
		if err := removeEntry(dir, entry.Name()); err != nil {
			dir.Close()
			return err
		}
	}
	if err := dir.Close(); err != nil {
		return err
	}
	return unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
}
