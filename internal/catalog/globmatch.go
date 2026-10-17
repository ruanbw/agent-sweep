package catalog

import (
	"path"
	"strings"
)

// MatchGlob 判断相对路径 rel 是否匹配声明里的 glob 模式。
//
// 支持 `*` / `?` / `[]` 与 `**`（匹配零到多段），按 `/` 分段匹配；前导 `./`
// 会被去掉。只做字符串匹配、不碰磁盘 —— 删除时用它重放声明，不能再遍历一次
// 文件系统（那既慢，也会把新的竞态窗口带进删除路径）。
func MatchGlob(pattern, rel string) bool {
	pattern = strings.TrimPrefix(filepathToSlash(pattern), "./")
	return matchGlobParts(strings.Split(pattern, "/"), strings.Split(filepathToSlash(rel), "/"))
}

func matchGlobParts(pattern, rel []string) bool {
	if len(pattern) == 0 {
		return len(rel) == 0
	}
	if pattern[0] == "**" {
		// `**` 匹配零到多段：逐个尝试吞掉 rel 的前缀。
		for i := 0; i <= len(rel); i++ {
			if matchGlobParts(pattern[1:], rel[i:]) {
				return true
			}
		}
		return false
	}
	if len(rel) == 0 {
		return false
	}
	ok, err := path.Match(pattern[0], rel[0])
	if err != nil || !ok {
		return false
	}
	return matchGlobParts(pattern[1:], rel[1:])
}

// filepathToSlash 只把反斜杠换成斜杠，不引入 path/filepath（保持 catalog 的
// 匹配逻辑纯字符串、跨平台一致）。
func filepathToSlash(p string) string {
	return strings.ReplaceAll(p, `\`, "/")
}
