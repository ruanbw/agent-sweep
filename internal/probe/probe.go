// Package probe 负责「目录里没有、但本机确实装了」的 Agent 发现。
//
// 和 catalog 的关系：catalog 是已知 Agent 的声明式名单，probe 往名单外找漏网的。
// 两者的判断依据刻意不同 —— catalog 相信人写的路径，probe 只相信目录里的内容签名
// （.jsonl 里的 role/content、SQLite 里的 session 表……）。只靠目录名白名单会把
// 「叫 data 的会话库」全漏掉，也会把每个叫 sessions 的空目录都报上来，所以目录名
// 只值 0.15 分，内容指纹才是主证据。
//
// 性能约束：这是一次同步的本地扫描，调用方通常直接挂在 HTTP handler 上，用户等
// 不起。整个 Discover 以 ~3 秒为硬预算：递归深度封顶、单目录条目封顶、文件头最多
// 读 8KB、SQLite 探测逐个带超时，超出预算就带着已有结果返回，绝不无限扫。
package probe

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/fsx"
)

type Candidate struct {
	Path         string       `json:"path"`
	Name         string       `json:"name"`
	Confidence   float64      `json:"confidence"` // 0..1
	SessionCount int          `json:"sessionCount"`
	Bytes        int64        `json:"bytes"`
	Evidence     []string     `json:"evidence"`
	Suggested    catalog.Spec `json:"suggested"`
}

const (
	// 8KB 足够覆盖一行会话 JSON 的头部字段；再大只是把图片/base64 读进来。
	headBytes = 8 * 1024

	// 单个候选最多报 2GB：UI 只需要量级，真正删除时还会重新统计；
	// 而 fsx.Size 对大目录是无上限递归，不能让它拖垮发现页。
	maxCandidateBytes = int64(2 << 30)

	// 0.35 是「至少命中一条主指纹」的分数线：
	// 光目录名 0.15、光 JSON 0.2 都不够；一条内容证据（0.35）或
	// 目录名 + JSON（0.15+0.2）才算数。这样只有空目录时不会制造噪音。
	minConfidence = 0.35
	maxCandidates = 20

	// 3 秒目标里给最后的体积统计/排序留 ~400ms。
	scanBudget = 2600 * time.Millisecond

	// 单目录最多看 200 项。Electron 的 node_modules、日志目录动辄上万项，
	// 真会话在不在前 200 项里对「发现」这个场景已经够用了。
	maxEntriesPerDir = 200
	// 全局目录数上限，防止 Application Support 里堆着几万个缓存目录时扫穿。
	maxDirs = 4000
	// 文件头读取总预算：每个 8KB，400 个 ≈ 3MB，仍在一次扫描的合理范围内。
	maxHeaderReads = 400
	// SQLite 探测总预算：打开/查询异常库可能很慢，宁可漏几个也不能卡住。
	maxDBProbes = 24
	// 会话表行数最多数 5000 行；再多对 UI 的「N 条会话」没有区分度。
	sessionRowLimit = 5000
)

// 目录名只是加分项，且只认这些「会话容器」惯用名。
var sessionDirNames = map[string]struct{}{
	"sessions":      {},
	"threads":       {},
	"conversations": {},
	"chats":         {},
	"history":       {},
	"projects":      {},
	"tasks":         {},
	"session-state": {},
	"transcripts":   {},
}

// 明确排除的目录名（统一小写比较）。它们要么是源码/依赖，要么是缓存/崩溃日志，
// 即便里面碰巧有 .jsonl 也不是「Agent 的会话正文」。
var excludedNames = map[string]struct{}{
	".git":           {},
	"node_modules":   {},
	"code cache":     {},
	"gpucache":       {},
	"blob_storage":   {},
	"service worker": {},
	"crashpad":       {},
	"logs":           {},
	"cache":          {},
	"caches":         {},
	"tmp":            {},
	"__pycache__":    {},
	".venv":          {},
	"venv":           {},
}

// 浏览器/系统缓存的厂商名。只在 ~/Library/Caches 下启用，
// 因为 Application Support 里的 Microsoft/Google 目录可能正是 IDE 数据。
var cacheVendorExact = map[string]struct{}{
	"google":        {},
	"microsoft":     {},
	"bravesoftware": {},
	"firefox":       {},
	"opera":         {},
	"vivaldi":       {},
	"chromium":      {},
	"safari":        {},
	"arc":           {},
}

var cacheVendorPrefixes = []string{
	"com.apple.",
	"com.google.",
	"com.microsoft.",
	"com.brave.",
	"com.mozilla.",
	"com.operasoftware.",
	"google",
	"microsoft",
	"bravesoftware",
	"firefox",
	"mozilla",
}

// discover 的入口。known 里所有 Spec 的 Roots（含 Env 展开）都视为已知，
// 连同它们的子目录一起跳过 —— 否则每个已知 Agent 都会在发现页再出现一次。
func Discover(known []catalog.Spec) ([]Candidate, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	if home == "" {
		return nil, fmt.Errorf("无法确定用户主目录")
	}
	home = filepath.Clean(home)

	w := &walker{
		home:     home,
		known:    knownRoots(known, home),
		visited:  make(map[string]struct{}),
		deadline: time.Now().Add(scanBudget),
	}
	w.run()
	return w.collect(), nil
}

// knownRoots 收集已知 Agent 的数据根。只保留看起来像绝对路径的项：
// 空串、`~` 本身或 `/` 一旦进集合，会把整个扫描范围吞掉。
func knownRoots(known []catalog.Spec, home string) []string {
	seen := make(map[string]struct{})
	var roots []string
	for i := range known {
		for _, raw := range known[i].RootPaths(nil) {
			p := filepath.Clean(catalog.Expand(raw))
			if p == "" || p == "." || p == home || p == string(filepath.Separator) {
				continue
			}
			if !filepath.IsAbs(p) {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			roots = append(roots, p)
		}
	}
	return roots
}

// dirNode 是递归时的中间态：保存这一层的直接内容，供父目录做「一级子目录」
// 指纹判断。children 只包含真正走进去的目录（被排除/已知的不会进来）。
type dirNode struct {
	path     string
	visible  []os.DirEntry
	children []*dirNode

	ownJSONL         []string // 直接位于本目录的 .jsonl
	ownJSONLVerified int      // 其中文件头通过内容校验的数量
	ownJSON          []string // 直接位于本目录的 .json
	ownJSONMatched   []string // 其中命中 messages/conversation/chat 的文件
	ownJSONMatches   int      // 命中数量（= len(ownJSONMatched)）
	ownDBs           []string // 直接位于本目录的 .db/.sqlite/.sqlite3
}

// dirInfo 是一个目录的指纹结论。父目录会把子目录的「直接内容」汇总进来，
// 所以它天然实现了「本目录或一级子目录」这条规则，而不需要额外再列一遍目录。
type dirInfo struct {
	path     string
	score    float64
	evidence []string

	// 会话单元的路径集合。父子目录会重复收录同一批文件，最终按路径并集
	// 去重；一目录一会话时，目录本身作为单元，里面的文件不重复计数。
	sessionFiles map[string]struct{}
	sessionDirs  map[string]struct{}

	jsonlCount    int
	jsonlVerified int
	ownJSONLCount int    // 只数本目录直接文件，用来决定 Glob 的起点
	jsonlStoreDir string // 命中 .jsonl 最多的那一层目录（建议 Glob 的起点）
	jsonlDirCount int    // 有多少层子目录各自直接装着 .jsonl
	jsonMatches   int
	jsonStoreDir  string // 命中 .json 最多的那一层目录
	jsonDirCount  int    // 有多少层子目录各自直接装着 .json
	sqlite        *sqliteHit
	shape         bool
}

type walker struct {
	home     string
	known    []string
	visited  map[string]struct{}
	deadline time.Time

	dirs       int
	headers    int
	dbProbes   int
	infos      []*dirInfo
	infoByPath map[string]*dirInfo
}

func (w *walker) run() {
	// 顺序有讲究：先扫更具体的目录，再扫 ~ 下的隐藏目录。
	// visited 保证同一路径只分析一次，具体入口能拿到更深的层数。
	w.scanChildren(filepath.Join("Library", "Application Support"), 3, false)
	w.scanChildren(filepath.Join("Library", "Caches"), 2, true)
	w.scanChildren(".config", 2, false)
	w.scanChildren(filepath.Join(".local", "share"), 2, false)
	w.scanHomeHidden()
}

// scanChildren 扫 base 下的一级子目录，每个子目录及其后代最多再看 maxDepth 层。
// 不存在（或不是目录）的 base 直接跳过，这是 macOS 上 Caches/Application Support
// 可能缺失时的正常分支。
func (w *walker) scanChildren(rel string, maxDepth int, inCache bool) {
	if w.expired() {
		return
	}
	base := filepath.Join(w.home, rel)
	if !fsx.IsDir(base) {
		return
	}
	for _, entry := range w.listEntries(base) {
		if w.expired() {
			return
		}
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		name := entry.Name()
		if isExcludedName(name) || isAppSelf(name) {
			continue
		}
		if inCache && isSystemCacheName(name) {
			continue
		}
		child := filepath.Join(base, name)
		if w.isKnown(child) {
			continue
		}
		w.walk(child, 0, maxDepth, inCache)
	}
}

// scanHomeHidden 只扫 ~ 下的一级隐藏目录（~/.xxx），再往下最多 2 层。
// 不扫非隐藏目录：~/Documents、~/src 这类是用户资料，不是 Agent 数据。
func (w *walker) scanHomeHidden() {
	if w.expired() {
		return
	}
	for _, entry := range w.listEntries(w.home) {
		if w.expired() {
			return
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		name := entry.Name()
		if name == "." || name == ".." || isExcludedName(name) || isAppSelf(name) {
			continue
		}
		child := filepath.Join(w.home, name)
		if w.isKnown(child) {
			continue
		}
		w.walk(child, 0, 2, false)
	}
}

// walk 是一趟深度受限的 DFS。先递归子目录再分析自己，父目录才能拿到子目录的
// 直接内容来判「一级子目录」指纹；同时也避免了给每个目录额外再 ReadDir 一次。
func (w *walker) walk(path string, depth, maxDepth int, inCache bool) *dirNode {
	if w.expired() || w.dirs >= maxDirs {
		return nil
	}
	if _, ok := w.visited[path]; ok {
		// 重叠入口（~ 下的 .config 与单独的 ~/.config/*）只分析一次。
		return nil
	}
	w.visited[path] = struct{}{}
	w.dirs++

	node := &dirNode{path: path, visible: w.visibleEntries(path, inCache)}
	if depth < maxDepth {
		for _, entry := range node.visible {
			if w.expired() {
				break
			}
			if !entry.IsDir() {
				continue
			}
			childPath := filepath.Join(path, entry.Name())
			if w.isKnown(childPath) {
				continue
			}
			if child := w.walk(childPath, depth+1, maxDepth, inCache); child != nil {
				node.children = append(node.children, child)
			}
		}
	}

	w.analyze(node)
	return node
}

// analyze 把一层目录的直接内容 + 直接子目录的直接内容折成指纹分。
func (w *walker) analyze(node *dirNode) {
	if w.expired() {
		return
	}

	for _, entry := range node.visible {
		if entry.IsDir() || !entry.Type().IsRegular() {
			continue
		}
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".jsonl":
			node.ownJSONL = append(node.ownJSONL, filepath.Join(node.path, entry.Name()))
		case ".json":
			node.ownJSON = append(node.ownJSON, filepath.Join(node.path, entry.Name()))
		case ".db", ".sqlite", ".sqlite3":
			node.ownDBs = append(node.ownDBs, filepath.Join(node.path, entry.Name()))
		}
	}
	sort.Strings(node.ownJSONL)
	sort.Strings(node.ownJSON)
	sort.Strings(node.ownDBs)

	// JSONL 内容校验：一个目录最多验 8 个文件头。
	// 只要有一个文件头像消息流，其余同目录的 .jsonl 基本可信；
	// 验 8 个是为了不被几十个会话文件吃掉全局读取预算。
	for i, file := range node.ownJSONL {
		if i >= 8 || !w.takeHeaderRead() {
			break
		}
		if head, err := fsx.ReadHead(file, headBytes); err == nil && containsAny(head, jsonlMarkers) {
			node.ownJSONLVerified++
		}
	}

	// .json 可能是索引、配置、package.json，误报率高，所以必须真的读到
	// messages/conversation/chat 才算；同样最多读 5 个。
	//
	// 只读 5 个是为了控制预算，但 SessionCount 不能只报 5：如果抽样的 5 个
	// 全部命中，就把同目录下所有 .json 都视为会话文件（真实案例：
	// billion-context/sessions/openai 下有 42 个 *.json）。抽样里混入配置
	// 时则保守地只数命中数。
	sampled, matched := 0, 0
	for i, file := range node.ownJSON {
		if i >= 5 || !w.takeHeaderRead() {
			break
		}
		sampled++
		if head, err := fsx.ReadHead(file, headBytes); err == nil && containsAny(head, jsonMarkers) {
			matched++
			node.ownJSONMatched = append(node.ownJSONMatched, file)
		}
	}
	if matched > 0 {
		if matched == sampled {
			node.ownJSONMatches = len(node.ownJSON)
			// 会话单元也用全量文件路径，否则 SessionCount 会停在抽样数上。
			node.ownJSONMatched = append([]string(nil), node.ownJSON...)
		} else {
			node.ownJSONMatches = matched
		}
	}

	var hit *sqliteHit
	if len(node.ownDBs) > 0 {
		for i, dbPath := range node.ownDBs {
			// 一个目录里通常只有一个真会话库，旁边多是 -wal/-shm 或索引库。
			// 探前两个即可，避免被一个装满 .db 的缓存目录拖住。
			if i >= 2 || w.dbProbes >= maxDBProbes || w.expired() {
				break
			}
			w.dbProbes++
			if h := probeSQLite(dbPath, w.deadline); h != nil {
				hit = h
				break
			}
		}
	}

	info := &dirInfo{
		path:          node.path,
		sqlite:        hit,
		ownJSONLCount: len(node.ownJSONL),
	}

	score := 0.0
	var evidence []string

	// 信号 1：目录名。只值 0.15，永远不足以单独越过门槛。
	if sessionLikeName(filepath.Base(node.path)) {
		score += 0.15
		evidence = append(evidence, fmt.Sprintf("目录名 %q 是常见的会话容器名", filepath.Base(node.path)))
	}

	// 信号 2：本目录或一级子目录下的 .jsonl，且文件头出现消息字段。
	// 汇总子目录的直接文件，正是为了覆盖 sessions/、threads/ 这一层。
	jsonlTotal := len(node.ownJSONL)
	jsonlVerified := node.ownJSONLVerified
	jsonlStoreDir := ""
	jsonlDirCount := 0
	if jsonlTotal > 0 {
		jsonlStoreDir = node.path
		jsonlDirCount = 1
	}
	jsonlBest := jsonlTotal
	for _, child := range node.children {
		childCount := len(child.ownJSONL)
		if childCount > 0 {
			jsonlTotal += childCount
			jsonlDirCount++
			if childCount > jsonlBest {
				jsonlBest = childCount
				jsonlStoreDir = child.path
			}
		}
		jsonlVerified += child.ownJSONLVerified
	}
	if jsonlTotal > 0 && jsonlVerified > 0 {
		score += 0.35
		// 5 个是「这里有批量会话，而不是一个偶发导出的文件」的经验分界；
		// 只加 0.1，避免单靠数量把噪音顶过门槛。
		if jsonlTotal >= 5 {
			score += 0.1
		}
		evidence = append(evidence, fmt.Sprintf("%d 个 .jsonl 的文件头含 role/content/messages", jsonlTotal))
		info.jsonlCount = jsonlTotal
		info.jsonlVerified = jsonlVerified
		info.jsonlStoreDir = jsonlStoreDir
		info.jsonlDirCount = jsonlDirCount
		for _, file := range node.ownJSONL {
			addSessionFile(info, file)
		}
		for _, child := range node.children {
			for _, file := range child.ownJSONL {
				addSessionFile(info, file)
			}
		}
	}

	// 信号 3：.json 会话文件。0.2 单独不够，需要叠加目录名（0.15）才到 0.35。
	jsonMatches := node.ownJSONMatches
	jsonStoreDir := ""
	jsonDirCount := 0
	jsonBest := jsonMatches
	if jsonMatches > 0 {
		jsonStoreDir = node.path
		jsonDirCount = 1
	}
	for _, child := range node.children {
		if child.ownJSONMatches > 0 {
			jsonMatches += child.ownJSONMatches
			jsonDirCount++
			if child.ownJSONMatches > jsonBest {
				jsonBest = child.ownJSONMatches
				jsonStoreDir = child.path
			}
		}
	}
	if jsonMatches > 0 {
		score += 0.2
		info.jsonMatches = jsonMatches
		info.jsonStoreDir = jsonStoreDir
		info.jsonDirCount = jsonDirCount
		evidence = append(evidence, fmt.Sprintf("%d 个 .json 的文件头含 messages/conversation/chat", jsonMatches))
		for _, file := range node.ownJSONMatched {
			addSessionFile(info, file)
		}
		for _, child := range node.children {
			for _, file := range child.ownJSONMatched {
				addSessionFile(info, file)
			}
		}
	}

	// 信号 4：SQLite。0.4 足够独立成立，因为「表名含 session/thread/...」
	// 比文件名可靠得多；打不开或没有此类表就静默跳过。
	if hit != nil {
		score += 0.4
		evidence = append(evidence, hit.evidence())
	}

	// 信号 5：一目录一会话。要求至少 2 个这样的子目录，且它们占全部子项
	// 的多数（> 50%）：这样 README/配置/索引文件不会把形态判断带偏。
	sessionDirs := 0
	for _, child := range node.children {
		if len(child.ownJSONL) > 0 || child.ownJSONMatches > 0 {
			sessionDirs++
		}
	}
	if len(node.visible) >= 3 && sessionDirs >= 2 && sessionDirs*2 > len(node.visible) {
		score += 0.15
		info.shape = true
		evidence = append(evidence, fmt.Sprintf("%d 个子目录含 .json/.jsonl，多数呈一目录一会话形态", sessionDirs))
		for _, child := range node.children {
			if len(child.ownJSONL) > 0 || child.ownJSONMatches > 0 {
				addSessionDir(info, child.path)
			}
		}
	}

	// 浮点加法会出现 0.35000000000000003 这类值，四舍五入到两位便于 UI 展示，
	// 也避免同一份证据在不同机器上显示成不同的尾数。
	score = math.Round(math.Min(1, score)*100) / 100
	info.score = score
	info.evidence = evidence

	// 容器目录（~、~/.config、~/Library/Application Support ……）本身不作为候选，
	// 但它的子目录已经在上面被递归分析了。
	if score > 0 && !w.isContainerPath(node.path) {
		w.infos = append(w.infos, info)
		if w.infoByPath == nil {
			w.infoByPath = make(map[string]*dirInfo)
		}
		w.infoByPath[node.path] = info
	}
}

// candidateGroup 是归并过程中的中间结果。path 是最终展示路径；
// merged=true 时它代表「共享同一父目录的一批会话单元」。
type candidateGroup struct {
	path         string
	members      []*dirInfo
	rep          *dirInfo
	confidence   float64
	sessionCount int
	suggested    catalog.Spec
	merged       bool
	unitCount    int
	summary      string // 合并候选的一句话证据
}

// collect 先按「同一个数据根」归并父子/兄弟候选，再把共享父目录的会话单元
// 合并成一条，最后排序截断。
//
// 为什么必须归并：~/.foo、~/.foo/sessions、~/.foo/sessions/<id> 会分别得分，
// 但它们只是同一款 Agent 的根、容器和单条会话。发现页要的是 Agent，不是文件树。
// 真实案例：~/Library/Application Support/Code/agentSessionData/<uuid>/session.db
// 有 100 多个 UUID 目录，每个都是一条会话；不合并会瞬间占满 20 条上限。
func (w *walker) collect() []Candidate {
	if w.infoByPath == nil {
		return []Candidate{}
	}

	qualifying := make(map[string]*dirInfo)
	for _, info := range w.infos {
		if info.score >= minConfidence {
			qualifying[info.path] = info
		}
	}
	if len(qualifying) == 0 {
		return []Candidate{}
	}

	// 每个候选先找到「最上层的合格祖先」：这样 .foo 和 .foo/sessions 会归到同一组。
	groups := make(map[string][]*dirInfo)
	for _, info := range w.infos {
		if info.score < minConfidence {
			continue
		}
		top := info.path
		for parent := filepath.Dir(info.path); parent != w.home && parent != "." && parent != string(filepath.Separator); parent = filepath.Dir(parent) {
			if _, ok := qualifying[parent]; ok {
				top = parent
			}
		}
		// 再用数据根兜底：只有 ~/.foo/sessions 与 ~/.foo/threads 合格、~/.foo
		// 自己不合格时，两者也应该并成一条。
		root := w.dataRoot(top)
		groups[root] = append(groups[root], info)
	}

	protos := make([]*candidateGroup, 0, len(groups))
	for root, members := range groups {
		members = dedupeInfos(members)
		rep := pickRepresentative(members)
		confidence := 0.0
		for _, member := range members {
			confidence = math.Max(confidence, member.score)
		}
		protos = append(protos, &candidateGroup{
			path:         rep.path,
			members:      members,
			rep:          rep,
			confidence:   confidence,
			sessionCount: countSessionUnits(members),
			suggested:    w.buildSpec(root, members),
		})
	}

	// 第二次归并：把共享同一父目录的「一目录一会话」候选合成一条。
	protos = w.mergeSiblings(protos)

	sort.SliceStable(protos, func(i, j int) bool {
		if protos[i].confidence != protos[j].confidence {
			return protos[i].confidence > protos[j].confidence
		}
		return protos[i].path < protos[j].path
	})
	if len(protos) > maxCandidates {
		protos = protos[:maxCandidates]
	}

	candidates := make([]Candidate, 0, len(protos))
	for _, proto := range protos {
		evidence := mergeEvidence(proto.members)
		if proto.merged && proto.summary != "" {
			// 用户一眼要能看出这条代表多少条会话，而不是又被列成一堆 UUID。
			evidence = append([]string{proto.summary}, evidence...)
		}
		var size int64
		if w.expired() {
			// 宁可报 0 并说明原因，也不让发现页因为一个超大目录卡过 3 秒。
			evidence = append(evidence, "扫描时间用尽，体积未统计")
		} else {
			size = fsx.Size(proto.path)
			if size > maxCandidateBytes {
				size = maxCandidateBytes
				evidence = append(evidence, "目录体积超过 2GB，按 2GB 上限报告")
			}
		}
		if len(evidence) > 10 {
			evidence = evidence[:10]
		}
		candidates = append(candidates, Candidate{
			Path:         proto.path,
			Name:         proto.suggested.Name,
			Confidence:   proto.confidence,
			SessionCount: proto.sessionCount,
			Bytes:        size,
			Evidence:     evidence,
			Suggested:    proto.suggested,
		})
	}
	return candidates
}

// mergeSiblings 处理「同一父目录下每个子目录都是一条会话」的形态。
//
// 只按父目录分组还不够：同一父目录下也可能放着两款不同 Agent 的数据。
// 因此额外要求 (a) 多数子目录都是候选、(b) 各候选的存储签名一致，
// 满足后才合成一条，Path/Name 取父目录，SessionCount 累加。
func (w *walker) mergeSiblings(protos []*candidateGroup) []*candidateGroup {
	buckets := make(map[string][]*candidateGroup)
	var order []string
	for _, proto := range protos {
		parent := filepath.Dir(proto.path)
		if _, ok := buckets[parent]; !ok {
			order = append(order, parent)
		}
		buckets[parent] = append(buckets[parent], proto)
	}

	out := make([]*candidateGroup, 0, len(protos))
	for _, parent := range order {
		bucket := buckets[parent]
		if len(bucket) < 2 || parent == w.home || w.isContainerPath(parent) ||
			!w.shouldMergeSiblings(parent, bucket) || !sameUnitSignature(bucket) {
			out = append(out, bucket...)
			continue
		}
		out = append(out, w.mergeSiblingGroup(parent, bucket))
	}
	return out
}

func (w *walker) shouldMergeSiblings(parent string, bucket []*candidateGroup) bool {
	// 候选本身得是「一条会话」的目录（直接装着 sqlite/jsonl/json），
	// 否则 sessions/threads 这类容器层可能把不同数据揉在一起。
	units := 0
	for _, proto := range bucket {
		if proto.rep != nil && isSessionUnit(proto.rep) {
			units++
		}
	}
	// 每个候选自己就装着 sqlite/jsonl/json（而不是靠子目录才得分），
	// 说明它们是同一种「一目录一会话」单元。此时即便扫描预算只来得及
	// 处理其中一部分（父目录下还有几十个没扫到），也应合并成一条：
	// 否则发现页会被同一批数据的已扫部分刷屏。父目录已由调用方保证
	// 不是 ~ / Application Support 这类公共容器。
	if units == len(bucket) {
		return true
	}
	if !sessionLikeName(filepath.Base(parent)) {
		return false
	}

	// 非「自包含单元」的容器候选，要求父目录下大部分子目录都在候选里，
	// 否则可能只是同一 App 父目录下两个碰巧都被识别的子项目。
	paths := make(map[string]struct{}, len(bucket))
	for _, proto := range bucket {
		paths[proto.path] = struct{}{}
	}
	childDirs, candidateDirs := 0, 0
	for _, entry := range w.listEntries(parent) {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || isExcludedName(name) || isAppSelf(name) {
			continue
		}
		childDirs++
		if _, ok := paths[filepath.Join(parent, name)]; ok {
			candidateDirs++
		}
	}
	return childDirs > 0 && candidateDirs*2 >= childDirs
}

// sameUnitSignature 要求同一批子目录的存储形态一致（同名 DB + 同表，或都是
// 直接放 JSONL 的目录）。UUID 目录里各放一个 session.db 是最典型的正例。
func sameUnitSignature(bucket []*candidateGroup) bool {
	signature := ""
	for _, proto := range bucket {
		if proto.rep == nil {
			return false
		}
		current := unitSignature(proto.rep)
		if current == "" {
			return false
		}
		if signature == "" {
			signature = current
		} else if signature != current {
			return false
		}
	}
	return signature != ""
}

func unitSignature(info *dirInfo) string {
	if info.sqlite != nil {
		return "sqlite:" + filepath.Base(info.sqlite.path) + ":" + strings.ToLower(info.sqlite.table)
	}
	if info.ownJSONLCount > 0 {
		return "jsonl"
	}
	if info.jsonMatches > 0 {
		return "json"
	}
	return ""
}

func isSessionUnit(info *dirInfo) bool {
	return info.sqlite != nil || info.ownJSONLCount > 0 || info.jsonMatches > 0
}

func (w *walker) mergeSiblingGroup(parent string, bucket []*candidateGroup) *candidateGroup {
	var members []*dirInfo
	confidence := 0.0
	for _, proto := range bucket {
		members = append(members, proto.members...)
		confidence = math.Max(confidence, proto.confidence)
	}
	members = dedupeInfos(members)

	return &candidateGroup{
		path:    parent,
		members: members,
		rep:     bucket[0].rep,
		// 一目录一会话：子目录数就是会话数。SQLite 表里可能一条会话有多行，
		// 把它们加起来会把「一条会话」重复计数，所以合并后以子目录数为准。
		sessionCount: len(bucket),
		confidence:   confidence,
		suggested:    w.buildMergedSpec(parent),
		merged:       true,
		unitCount:    len(bucket),
		summary:      mergedSummary(bucket),
	}
}

// mergedSummary 生成「N 个形如 uuid 的子目录，每个含 session.db，表 xxx」这种
// 人能直接看懂的说明。父目录下每个子目录都是一条会话，比罗列 20 个 UUID 有用。
func mergedSummary(bucket []*candidateGroup) string {
	allUUID := true
	dbName, table := "", ""
	consistentDB := true
	for _, proto := range bucket {
		if !looksLikeUUID(filepath.Base(proto.path)) {
			allUUID = false
		}
		if proto.rep == nil || proto.rep.sqlite == nil {
			consistentDB = false
			continue
		}
		currentName := filepath.Base(proto.rep.sqlite.path)
		currentTable := proto.rep.sqlite.table
		if dbName == "" {
			dbName, table = currentName, currentTable
		} else if dbName != currentName || table != currentTable {
			consistentDB = false
		}
	}

	detail := "每个含会话文件"
	if consistentDB && dbName != "" {
		detail = "每个含 " + dbName
		if table != "" {
			detail += "，表 " + table
		}
	}
	if allUUID {
		return fmt.Sprintf("%d 个形如 uuid 的子目录，%s", len(bucket), detail)
	}
	return fmt.Sprintf("%d 个子目录，%s", len(bucket), detail)
}

// looksLikeUUID 判断目录名是不是 8-4-4-4-12 的十六进制 UUID。
// 只用于生成给人看的证据文案，判断错也不影响合并本身。
func looksLikeUUID(name string) bool {
	if len(name) != 36 {
		return false
	}
	for i, r := range name {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// buildMergedSpec 给「父目录 + 若干会话子目录」生成草稿。
//
// Roots 就是父目录，Store 用 dir + `*`：每个子目录本身是一条会话，
// 里面是 session.db 还是 chat.jsonl 都不影响这层判断。catalog 没有
// 「DB 通配」这种表达，一个 sqlite Store 只能指向一个库文件，所以
// 多库形态必须退成 dir 才能扫到全部子目录。
func (w *walker) buildMergedSpec(path string) catalog.Spec {
	base := strings.TrimLeft(filepath.Base(path), ".")
	if base == "" {
		base = "discovered"
	}
	return catalog.Spec{
		ID:     kebabCase(base),
		Name:   displayName(base),
		Group:  catalog.GroupOther,
		Roots:  []string{w.tilde(path)},
		Stores: []catalog.Store{{Kind: "dir", Glob: "*"}},
		Custom: true,
		Note:   "由内容指纹自动合并，一个子目录视为一条会话；保存前请确认。",
	}
}

// countSessionUnits 统计一组目录里不重复的会话单元：文件按路径去重，
// 一目录一会话时目录本身算一个单元、里面的文件不再重复计数；
// 纯 SQLite 的会话数取行数（组内取最大值，避免库与导出文件双算）。
func countSessionUnits(members []*dirInfo) int {
	files := make(map[string]struct{})
	dirs := make(map[string]struct{})
	sqliteRows := 0
	for _, member := range members {
		for file := range member.sessionFiles {
			files[file] = struct{}{}
		}
		for dir := range member.sessionDirs {
			dirs[dir] = struct{}{}
		}
		if member.sqlite != nil {
			sqliteRows = maxInt(sqliteRows, member.sqlite.rows)
		}
	}
	count := len(dirs)
	for file := range files {
		inside := false
		for dir := range dirs {
			if isDescendant(file, dir) {
				inside = true
				break
			}
		}
		if !inside {
			count++
		}
	}
	return maxInt(count, sqliteRows)
}

func addSessionFile(info *dirInfo, path string) {
	if info.sessionFiles == nil {
		info.sessionFiles = make(map[string]struct{})
	}
	info.sessionFiles[path] = struct{}{}
}

func addSessionDir(info *dirInfo, path string) {
	if info.sessionDirs == nil {
		info.sessionDirs = make(map[string]struct{})
	}
	info.sessionDirs[path] = struct{}{}
}

// pickRepresentative 选分最高的那一项；同分时浅的优先（更像 Agent 根，
// 而不是某一条会话）。
func pickRepresentative(members []*dirInfo) *dirInfo {
	rep := members[0]
	for _, member := range members[1:] {
		if member.score > rep.score || (member.score == rep.score && depthOf(member.path) < depthOf(rep.path)) {
			rep = member
		}
	}
	return rep
}

func dedupeInfos(members []*dirInfo) []*dirInfo {
	seen := make(map[string]struct{}, len(members))
	out := members[:0]
	for _, member := range members {
		if _, ok := seen[member.path]; ok {
			continue
		}
		seen[member.path] = struct{}{}
		out = append(out, member)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if depthOf(out[i].path) != depthOf(out[j].path) {
			return depthOf(out[i].path) < depthOf(out[j].path)
		}
		return out[i].path < out[j].path
	})
	return out
}

// buildSpec 生成一份「能直接 POST /api/adapters」的草稿。
//
// Roots 取数据根而不是代表路径：用户保存后，扫描引擎从 Roots 展开 Glob，
// 因此 Glob/DB 必须相对 Roots 计算，否则存进去的第一条会话就扫不到。
func (w *walker) buildSpec(root string, members []*dirInfo) catalog.Spec {
	base := strings.TrimLeft(filepath.Base(root), ".")
	if base == "" {
		base = "discovered"
	}

	spec := catalog.Spec{
		ID:     kebabCase(base),
		Name:   displayName(base),
		Group:  catalog.GroupOther,
		Roots:  []string{w.tilde(root)},
		Custom: true,
		Note:   "由内容指纹自动发现，Roots 与 Stores 为推断值，保存前请确认。",
	}

	// 有 SQLite 就优先 SQLite：库里的表名/主键是比文件扩展名更硬的证据，
	// 而且同一个 Agent 常常同时留 DB 和导出的 JSONL，DB 才是主存储。
	if hit := pickSQLite(members); hit != nil {
		if rel, err := filepath.Rel(root, hit.path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			spec.Stores = []catalog.Store{{
				Kind:     "sqlite",
				DB:       filepath.ToSlash(rel),
				Table:    hit.table,
				IDCol:    hit.idCol,
				TitleCol: hit.titleCol,
				TimeCol:  hit.timeCol,
				CWDCol:   hit.cwdCol,
			}}
			return spec
		}
	}

	// 文件型会话：Glob 必须从「真正装着会话文件的那一层」反推，不能看到
	// sessions/ 就直接给 dir。billion-context 就是反例：正文在
	// sessions/openai/*.json，dir + sessions/* 会把整个 openai 当成 1 条会话。
	if file := pickFileStore(members); file != nil {
		// 只有当「每个会话目录里各有一个会话文件」时，目录本身才是会话单元
		// （Cline tasks/<id>/api_conversation_history.json）。此时用 dir，
		// Glob 指到会话目录这一层，而不是文件所在目录的再下一层。
		if file.dir != root && file.dirCount >= 2 && file.count == file.dirCount {
			container := filepath.Dir(file.dir)
			spec.Stores = []catalog.Store{{Kind: "dir", Glob: globForDirs(root, container)}}
			return spec
		}
		ext := ".jsonl"
		if file.kind == "json" {
			ext = ".json"
		}
		spec.Stores = []catalog.Store{{Kind: file.kind, Glob: globForFiles(root, file.dir, ext)}}
		return spec
	}

	// 没有任何可识别的文件型会话，但存在一目录一会话形态（会话目录里放的
	// 是压缩包、自定义扩展名等），才退回 dir。
	if storeDir, ok := pickShapeStoreDir(members, root); ok {
		spec.Stores = []catalog.Store{{Kind: "dir", Glob: globForDirs(root, storeDir)}}
		return spec
	}

	spec.Stores = []catalog.Store{{Kind: "dir", Glob: "*"}}
	return spec
}

// fileStore 描述「会话是文件」的命中：哪个目录、多少文件、分布在几层目录。
type fileStore struct {
	kind     string // jsonl | json
	dir      string // 命中文件最多的那一层目录
	count    int    // 会话文件总数
	dirCount int    // 有多少个目录各自直接装着命中文件
}

// pickFileStore 先 jsonl 后 json：两者同时存在时 jsonl 更像正文，
// json 更常见的是索引/配置。计数取所有成员里的最大值，避免父子重复。
func pickFileStore(members []*dirInfo) *fileStore {
	var best *fileStore
	for _, member := range members {
		if member.jsonlCount <= 0 || member.jsonlStoreDir == "" {
			continue
		}
		candidate := &fileStore{
			kind:     "jsonl",
			dir:      member.jsonlStoreDir,
			count:    member.jsonlCount,
			dirCount: member.jsonlDirCount,
		}
		if best == nil || candidate.count > best.count {
			best = candidate
		}
	}
	if best != nil {
		return best
	}
	for _, member := range members {
		if member.jsonMatches <= 0 || member.jsonStoreDir == "" {
			continue
		}
		candidate := &fileStore{
			kind:     "json",
			dir:      member.jsonStoreDir,
			count:    member.jsonMatches,
			dirCount: member.jsonDirCount,
		}
		if best == nil || candidate.count > best.count {
			best = candidate
		}
	}
	return best
}

// globForFiles 把「文件所在目录」翻译成相对 Roots 的 Glob。
//
// 结构性目录名（sessions/projects/tasks/chats/tmp…）保留字面量，其余层级
// 用 * 表示「每个项目/会话各一层」。于是：
//   - sessions/openai/*.json → sessions/*/*.json
//   - projects/<hash>/*.jsonl → projects/*/*.jsonl
//   - 文件直接在数据根 → *.jsonl
func globForFiles(root, dir, ext string) string {
	rel, ok := relUnder(root, dir)
	if !ok {
		return "**/*" + ext
	}
	if rel == "." {
		return "*" + ext
	}
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		if !structuralDirName(part) {
			parts[i] = "*"
		}
	}
	return strings.Join(parts, "/") + "/*" + ext
}

// globForDirs 指向「会话目录」这一层本身（dir Store 一条会话 = 一个目录）。
// 例：container=tasks → tasks/*；sessions/<proj> → sessions/*/*。
func globForDirs(root, container string) string {
	rel, ok := relUnder(root, container)
	if !ok {
		return "**"
	}
	if rel == "." {
		return "*"
	}
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		if !structuralDirName(part) {
			parts[i] = "*"
		}
	}
	return strings.Join(parts, "/") + "/*"
}

func relUnder(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// structuralDirName 是「目录结构名」而非「项目/会话变量名」的白名单。
// 这些名字在 Glob 里保留字面量，其余目录名一律替换成 *。
func structuralDirName(name string) bool {
	normalized := strings.ToLower(strings.TrimLeft(name, "."))
	if normalized == "tmp" {
		return true
	}
	_, ok := sessionDirNames[normalized]
	return ok
}

// pickSQLite 在组内挑最能代表会话的库：表名越像会话表、主键/标题/时间列越全越好。
func pickSQLite(members []*dirInfo) *sqliteHit {
	var best *sqliteHit
	for _, member := range members {
		hit := member.sqlite
		if hit == nil {
			continue
		}
		if best == nil || hit.quality > best.quality ||
			(hit.quality == best.quality && hit.rows > best.rows) ||
			(hit.quality == best.quality && hit.rows == best.rows && hit.path < best.path) {
			best = hit
		}
	}
	return best
}

// pickShapeStoreDir 是 dir 兜底时的 Glob 起点。和 JSONL 同样的思路，
// 但依据是一目录一会话形态或 .json 会话文件。
func pickShapeStoreDir(members []*dirInfo, root string) (string, bool) {
	for _, member := range members {
		if member.path == root {
			if member.shape || member.jsonMatches > 0 {
				return root, true
			}
			break
		}
	}
	tops := topLevelWith(members, root, func(m *dirInfo) bool {
		return m.shape || m.jsonMatches > 0
	})
	if len(tops) == 1 {
		return tops[0].path, true
	}
	if len(tops) > 1 {
		return root, true
	}
	return root, false
}

// topLevelWith 返回满足条件的成员中「没有被另一个成员包含」的那些。
// 例：sessions 与 sessions/2026 都命中时，只保留 sessions。
func topLevelWith(members []*dirInfo, root string, keep func(*dirInfo) bool) []*dirInfo {
	var filtered []*dirInfo
	for _, member := range members {
		if member.path == root || !keep(member) {
			continue
		}
		filtered = append(filtered, member)
	}
	var tops []*dirInfo
	for _, member := range filtered {
		covered := false
		for _, other := range filtered {
			if other.path == member.path {
				continue
			}
			if isDescendant(member.path, other.path) {
				covered = true
				break
			}
		}
		if !covered {
			tops = append(tops, member)
		}
	}
	sort.SliceStable(tops, func(i, j int) bool { return tops[i].path < tops[j].path })
	return tops
}

func mergeEvidence(members []*dirInfo) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, member := range members {
		for _, line := range member.evidence {
			if _, ok := seen[line]; ok {
				continue
			}
			seen[line] = struct{}{}
			out = append(out, line)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// dataRoot 决定 Suggested.Roots。sessions/threads 这类目录通常只是 Agent 数据
// 根下面的一层容器，往上退一级才适合当 Roots；但如果父目录是 ~、~/Library/
// Application Support 这种公共容器，退上去会让扫描范围爆炸，所以原地不动。
func (w *walker) dataRoot(path string) string {
	if !sessionLikeName(filepath.Base(path)) {
		return path
	}
	parent := filepath.Dir(path)
	if parent == path || w.isContainerPath(parent) {
		return path
	}
	return parent
}

func (w *walker) isContainerPath(path string) bool {
	containers := []string{
		w.home,
		filepath.Join(w.home, "Library"),
		filepath.Join(w.home, "Library", "Application Support"),
		filepath.Join(w.home, "Library", "Caches"),
		filepath.Join(w.home, ".config"),
		filepath.Join(w.home, ".local"),
		filepath.Join(w.home, ".local", "share"),
		filepath.Join(w.home, ".cache"),
	}
	for _, container := range containers {
		if path == container {
			return true
		}
	}
	return false
}

func (w *walker) isKnown(path string) bool {
	path = filepath.Clean(path)
	for _, root := range w.known {
		if path == root || isDescendant(path, root) {
			return true
		}
	}
	return false
}

// listEntries 只取目录前 200 项；用 File.ReadDir(limit) 而不是 os.ReadDir，
// 后者会把整个目录（可能是几万项）读进内存再截断。
func (w *walker) listEntries(path string) []os.DirEntry {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	entries, err := file.ReadDir(maxEntriesPerDir)
	if err != nil && len(entries) == 0 {
		return nil
	}
	// 目录顺序不稳定会让「探测到的第一个表/文件」漂移，排序保证结果可复现。
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries
}

func (w *walker) visibleEntries(path string, inCache bool) []os.DirEntry {
	all := w.listEntries(path)
	out := make([]os.DirEntry, 0, len(all))
	for _, entry := range all {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || isExcludedName(name) || isAppSelf(name) {
			continue
		}
		if inCache && isSystemCacheName(name) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func (w *walker) expired() bool { return time.Now().After(w.deadline) }

func (w *walker) takeHeaderRead() bool {
	if w.headers >= maxHeaderReads || w.expired() {
		return false
	}
	w.headers++
	return true
}

func (w *walker) tilde(path string) string {
	if path == w.home {
		return "~"
	}
	if rel, err := filepath.Rel(w.home, path); err == nil &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return "~/" + filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
}

func sessionLikeName(name string) bool {
	normalized := strings.ToLower(strings.TrimLeft(name, "."))
	_, ok := sessionDirNames[normalized]
	return ok
}

func isExcludedName(name string) bool {
	_, ok := excludedNames[strings.ToLower(name)]
	return ok
}

func isAppSelf(name string) bool {
	return strings.EqualFold(name, "AgentSweep")
}

func isSystemCacheName(name string) bool {
	lower := strings.ToLower(name)
	if _, ok := cacheVendorExact[lower]; ok {
		return true
	}
	for _, prefix := range cacheVendorPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func isDescendant(path, ancestor string) bool {
	return path != ancestor && strings.HasPrefix(path, ancestor+string(filepath.Separator))
}

// 预编译成 []byte：一次发现最多读 400 个文件头，避免每个文件都做
// []byte → string 的整块拷贝。
var (
	jsonlMarkers = [][]byte{[]byte(`"role"`), []byte(`"content"`), []byte(`"messages"`)}
	jsonMarkers  = [][]byte{[]byte(`"messages"`), []byte(`"conversation"`), []byte(`"chat"`)}
)

func containsAny(data []byte, needles [][]byte) bool {
	for _, needle := range needles {
		if bytes.Contains(data, needle) {
			return true
		}
	}
	return false
}

func depthOf(path string) int {
	return strings.Count(filepath.Clean(path), string(filepath.Separator))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// kebabCase 生成稳定 ID：去前导点、转小写、非字母数字折叠成单个短横线。
// 例：`.FooAgent` → `fooagent`，`Some Agent` → `some-agent`。
func kebabCase(value string) string {
	var builder strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimLeft(value, ".")) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			builder.WriteRune(r)
			lastDash = false
		case !lastDash && builder.Len() > 0:
			builder.WriteByte('-')
			lastDash = true
		}
	}
	id := strings.Trim(builder.String(), "-")
	if id == "" {
		return "discovered"
	}
	return id
}

func displayName(base string) string {
	value := []rune(strings.TrimLeft(base, "."))
	if len(value) == 0 {
		return "Discovered"
	}
	value[0] = unicode.ToUpper(value[0])
	return string(value)
}
