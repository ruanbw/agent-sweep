// 本文件负责「删除前重放声明」：确认一条会话的每个待删路径确实属于它。
//
// 删除只按 session.Paths 执行，而 Paths 是扫描产物。适配器在扫描之后被用户改
// 过、或内存里的 Paths 被别处污染时，删除会按一份声明解释不了的路径清单动数据。
// 这里用同一份 spec 的 Store glob 与 Assoc 模板做交叉校验。
package clean

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// checkPathDeclared 确认会话的每条 Paths 都能由 spec 的 Store/Assoc 声明解释。
//
// 规则：
//   - spec 没有 Stores 时跳过（旧适配器、测试夹具里常见）；
//   - 相对 home 的路径命中任一 Store.Glob 即通过；
//   - 或命中某条 Assoc 模板（`{id}` 替换后）：以 `/` 开头按 home 根拼，否则按
//     该会话主路径（Paths[0]）所在目录拼，结果与待删路径逐字相等。
//
// 只做词法匹配：越界由 checkPathInHome 负责，这里只回答「这条路径属于本会话吗」。
// 不遍历文件系统 —— 删除路径上再加一次 ReadDir 只会引入新的竞态窗口。
func checkPathDeclared(spec catalog.Spec, home string, s session.Session) error {
	if len(spec.Stores) == 0 {
		return nil
	}
	mainDir := ""
	if len(s.Paths) > 0 {
		mainDir = filepath.Dir(s.Paths[0])
	}
	for _, path := range s.Paths {
		if pathDeclaredBySpec(spec, home, mainDir, s.ID, path) {
			continue
		}
		return fmt.Errorf(
			"路径 %s 无法由 Agent %s 的 Store/Assoc 声明解释（可用声明示例：%s）；为避免误删，整条会话已跳过",
			path, spec.ID, declaredPatternExamples(spec))
	}
	return nil
}

// pathDeclaredBySpec 判断单条路径能否被声明解释。
func pathDeclaredBySpec(spec catalog.Spec, home, mainDir, id, path string) bool {
	// Store.Glob：相对数据根的路径。
	if relative, err := filepath.Rel(home, path); err == nil &&
		relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(relative) {
		rel := filepath.ToSlash(relative)
		for _, store := range spec.Stores {
			if store.Glob != "" && catalog.MatchGlob(store.Glob, rel) {
				return true
			}
		}
	}

	// Store.Assoc：`{id}` 替换后按目录拼接，逐字比较。
	cleaned := filepath.Clean(path)
	for _, store := range spec.Stores {
		for _, template := range store.Assoc {
			assoc := strings.ReplaceAll(template, "{id}", id)
			var candidate string
			switch {
			case strings.HasPrefix(assoc, "/"):
				candidate = filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(assoc, "/")))
			case mainDir != "":
				candidate = filepath.Join(mainDir, filepath.FromSlash(assoc))
			default:
				continue
			}
			if filepath.Clean(candidate) == cleaned {
				return true
			}
		}
	}
	return false
}

// declaredPatternExamples 给出声明里的代表性 pattern，方便用户看出是哪条声明
// 解释不了当前路径。
func declaredPatternExamples(spec catalog.Spec) string {
	const maxExamples = 4
	var examples []string
	for _, store := range spec.Stores {
		if store.Glob != "" {
			examples = append(examples, store.Glob)
		}
		examples = append(examples, store.Assoc...)
		if len(examples) >= maxExamples {
			break
		}
	}
	if len(examples) == 0 {
		return "（声明里没有 glob/assoc）"
	}
	if len(examples) > maxExamples {
		examples = examples[:maxExamples]
	}
	return strings.Join(examples, "、")
}
