package catalog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
)

// AppDir 返回应用自己的数据目录：`~/Library/Application Support/AgentSweep`。
// 只放用户配置、备份与日志 —— 绝不在里面放任何 Agent 的会话。
func AppDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".agentsweep"
	}
	return filepath.Join(home, "Library", "Application Support", "AgentSweep")
}

// AdaptersPath 是用户自定义适配器文件。放 JSON 而不是内置进二进制，
// 是因为「发现一个没收录的 Agent」是运行时才知道的事。
func AdaptersPath() string { return filepath.Join(AppDir(), "adapters.json") }

// All 返回内置 + 用户自定义的完整目录。同 ID 时用户版本覆盖内置。
func All() []Spec {
	custom, _ := LoadCustom()
	byID := map[string]Spec{}
	order := make([]string, 0, len(custom))
	for _, spec := range Builtin() {
		byID[spec.ID] = spec
		order = append(order, spec.ID)
	}
	for _, spec := range custom {
		if spec.ID == "" {
			continue
		}
		// 自定义项放在最后，但覆盖同 ID 的内置项（用户改过的路径优先）。
		if _, exists := byID[spec.ID]; !exists {
			order = append(order, spec.ID)
		}
		byID[spec.ID] = spec
	}
	out := make([]Spec, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

// Find 按 ID 取一条目录声明。
func Find(id string) (Spec, bool) {
	for _, spec := range All() {
		if spec.ID == id {
			return spec, true
		}
	}
	return Spec{}, false
}

// LoadCustom 读用户适配器。文件不存在或坏了都返回空 —— 用户配置坏了
// 不该让应用起不来，最多是自定义的 Agent 消失。
func LoadCustom() ([]Spec, error) {
	data, err := os.ReadFile(AdaptersPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var specs []Spec
	if err := json.Unmarshal(data, &specs); err != nil {
		return nil, err
	}
	for i := range specs {
		specs[i].Custom = true
	}
	return specs, nil
}

// SaveCustom 覆盖写用户适配器。先写临时文件再 rename，避免半截 JSON。
func SaveCustom(specs []Spec) error {
	if err := os.MkdirAll(AppDir(), 0o755); err != nil {
		return err
	}
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].ID < specs[j].ID })
	data, err := json.MarshalIndent(specs, "", "  ")
	if err != nil {
		return err
	}
	tmp := AdaptersPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, AdaptersPath())
}

// AddCustom 新增或覆盖一条用户适配器。
func AddCustom(spec Spec) error {
	spec.Custom = true
	existing, _ := LoadCustom()
	replaced := false
	for i := range existing {
		if existing[i].ID == spec.ID {
			existing[i] = spec
			replaced = true
			break
		}
	}
	if !replaced {
		existing = append(existing, spec)
	}
	return SaveCustom(existing)
}

// RemoveCustom 删掉一条用户适配器。内置项删不掉（也不该删）：
// 返回 false 表示这不是用户自定义项。
func RemoveCustom(id string) (bool, error) {
	existing, _ := LoadCustom()
	out := make([]Spec, 0, len(existing))
	removed := false
	for _, spec := range existing {
		if spec.ID == id {
			removed = true
			continue
		}
		out = append(out, spec)
	}
	if !removed {
		return false, nil
	}
	return true, SaveCustom(out)
}
