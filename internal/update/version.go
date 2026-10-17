package update

import (
	"errors"
	"strconv"
	"strings"
)

// Version 是一个宽松的语义化版本号。
//
// 「宽松」是故意的：发布标签是 v0.1.0，开发构建可能是 0.1.0-3-gabc123-dirty，
// 用户手里也可能是旧版本留下的 0.1。严格按 semver 解析这些值会让「能不能升级」
// 变成一个猜谜游戏，而这里只需要回答一个问题：哪个更新。
type Version struct {
	Major, Minor, Patch int
	Pre                 []string
	Raw                 string
}

// ErrInvalidVersion 表示这串东西根本不是版本号（比如 "dev"）。
var ErrInvalidVersion = errors.New("不是合法版本号")

// ParseVersion 接受可选前缀 v、缺省的 minor / patch，以及 -prerelease 与 +build 元数据。
//
// 缺省补 0：`v1` 与 `1.0.0` 必须比较相等，否则 tag 写少一段就会永远「有更新」。
func ParseVersion(raw string) (Version, error) {
	v := Version{Raw: raw}
	text := strings.TrimSpace(raw)
	text = strings.TrimPrefix(text, "v")
	text = strings.TrimPrefix(text, "V")
	if text == "" {
		return v, ErrInvalidVersion
	}
	if index := strings.IndexByte(text, '+'); index >= 0 {
		text = text[:index]
	}
	if index := strings.IndexByte(text, '-'); index >= 0 {
		pre := text[index+1:]
		text = text[:index]
		if pre != "" {
			v.Pre = strings.Split(pre, ".")
		}
	}
	parts := strings.Split(text, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return v, ErrInvalidVersion
	}
	numbers := []*int{&v.Major, &v.Minor, &v.Patch}
	for index, part := range parts {
		number, ok := parseNumber(part)
		if !ok {
			return v, ErrInvalidVersion
		}
		*numbers[index] = number
	}
	return v, nil
}

// parseNumber 只接受纯数字。strconv.Atoi 会吞掉 "+1"，版本号里不该出现这种写法。
func parseNumber(text string) (int, bool) {
	if text == "" {
		return 0, false
	}
	for index := 0; index < len(text); index++ {
		if text[index] < '0' || text[index] > '9' {
			return 0, false
		}
	}
	number, err := strconv.Atoi(text)
	if err != nil {
		return 0, false
	}
	return number, true
}

// Compare 返回 -1 / 0 / 1，语义同 strings.Compare。
//
// 带 prerelease 的版本小于同号的正式版：0.2.0-rc.1 < 0.2.0。
func (v Version) Compare(other Version) int {
	if result := compareInt(v.Major, other.Major); result != 0 {
		return result
	}
	if result := compareInt(v.Minor, other.Minor); result != 0 {
		return result
	}
	if result := compareInt(v.Patch, other.Patch); result != 0 {
		return result
	}
	switch {
	case len(v.Pre) == 0 && len(other.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1
	case len(other.Pre) == 0:
		return -1
	}
	for index := 0; index < len(v.Pre) && index < len(other.Pre); index++ {
		if result := comparePre(v.Pre[index], other.Pre[index]); result != 0 {
			return result
		}
	}
	return compareInt(len(v.Pre), len(other.Pre))
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// comparePre 按 semver 的规则比 prerelease：纯数字段按数值比，字母段按字典序，数字段更小。
func comparePre(a, b string) int {
	aNumber, aOK := parseNumber(a)
	bNumber, bOK := parseNumber(b)
	switch {
	case aOK && bOK:
		return compareInt(aNumber, bNumber)
	case aOK:
		return -1
	case bOK:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// IsNewer 回答「latest 是否值得安装」。
//
// current 解析不出来（dev、本地构建）时一律返回 true：开发构建不可能通过版本号
// 判断新旧，让它能看到最新发布比让它永远沉默更有用。
func IsNewer(current, latest string) bool {
	latestVersion, err := ParseVersion(latest)
	if err != nil {
		return false
	}
	currentVersion, err := ParseVersion(current)
	if err != nil {
		return true
	}
	return latestVersion.Compare(currentVersion) > 0
}
