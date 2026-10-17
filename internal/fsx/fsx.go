// Package fsx 是扫描/清理共用的文件系统原语。
//
// 三件事只在这里实现一份，避免十几个适配器各写一遍后漂移：
//  1. 带 `**` 的 glob（标准库的 filepath.Glob 不支持递归）
//  2. 求体积（跳过隐藏项与符号链接，避免把 .git 算进「可回收空间」）
//  3. 安全删除（默认进废纸篓，可撤销）
package fsx

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Glob 在 root 下展开 pattern，支持 `*`（单段）与 `**`（任意层）。
//
// 只返回真实存在的路径，按字典序排 —— 扫描结果的顺序要稳定，
// 否则同一个会话在两次扫描之间会换位置，用户勾选到的东西会跳。
func Glob(root, pattern string) []string {
	if root == "" {
		return nil
	}
	pattern = filepath.ToSlash(strings.TrimPrefix(pattern, "./"))
	segments := strings.Split(pattern, "/")
	var out []string
	walk(root, segments, 0, &out)
	sortStrings(out)
	return out
}

func walk(dir string, segments []string, index int, out *[]string) {
	if index == len(segments) {
		*out = append(*out, dir)
		return
	}
	seg := segments[index]
	switch seg {
	case "", ".":
		walk(dir, segments, index+1, out)
	case "**":
		// `**` 可以匹配零层，所以先走一次当前层；再逐个子目录往下。
		walk(dir, segments, index+1, out)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			walk(filepath.Join(dir, entry.Name()), segments, index, out)
		}
	default:
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			ok, err := filepath.Match(seg, entry.Name())
			if err != nil || !ok {
				continue
			}
			walk(filepath.Join(dir, entry.Name()), segments, index+1, out)
		}
	}
}

// Size 递归求一个文件或目录的字节数。
//
// 跳过隐藏项与符号链接：`.git` / `.DS_Store` 不属于会话正文，
// 算进去会让「预计释放」虚高；跟随符号链接会在目录成环时无限递归。
func Size(path string) int64 {
	info, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	if !info.IsDir() {
		if info.Mode().IsRegular() {
			return info.Size()
		}
		return 0
	}
	var total int64
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		child := filepath.Join(path, entry.Name())
		info, err := os.Lstat(child)
		if err != nil {
			continue
		}
		switch {
		case info.IsDir():
			total += Size(child)
		case info.Mode().IsRegular():
			total += info.Size()
		}
	}
	return total
}

// Exists 判断路径是否存在（不区分文件/目录）。
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// IsDir 判断路径是否是目录。
func IsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// ReadHead 读取文件前 limit 字节。会话文件动辄几十 MB，
// 标题 / 项目路径全在头几行，扫全量是纯浪费。
func ReadHead(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, limit)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, err
	}
	return buf[:n], nil
}

// WriteFileAtomic 先写同目录临时文件再 rename：中途崩溃不会留下半截 JSON。
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
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
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func sortStrings(values []string) {
	// 小数组插入排序足够，避免为了排序引一个包进来。
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
