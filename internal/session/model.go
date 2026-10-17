// Package session 定义全应用唯一的会话模型，以及把各种落盘格式归一成它的解析逻辑。
//
// 为什么要归一：市面上的 Agent 有 JSONL、有 JSON、有 SQLite、有「一个目录就是一个会话」。
// 但用户要做的决定只有一个 —— 「删哪些」。所以扫描层往上一律只暴露 Session，
// UI 层完全不需要知道某个 Agent 用的是哪种存储。
package session

import (
	"time"
)

// Session 一条会话。
type Session struct {
	// ID 是会话在它自己 Agent 里的标识，用于裁索引、去重。
	ID string `json:"id"`
	// UID 是跨 Agent 唯一的键（agentID + ID）。列表勾选、删除都以它为键。
	UID       string    `json:"uid"`
	AgentID   string    `json:"agentId"`
	Title     string    `json:"title"`
	Project   string    `json:"project"`
	UpdatedAt time.Time `json:"updatedAt"`
	Messages  int       `json:"messages"`
	Bytes     int64     `json:"bytes"`
	Snippet   string    `json:"snippet"`
	// Paths 是这条会话真正占盘的文件/目录。文件型 Agent 删除时逐个进废纸篓；
	// SQLite 型为空，删除走删行。
	Paths []string `json:"paths"`
	// Storage 标记来源形态：file / sqlite / dir。
	Storage string `json:"storage"`
	// Active 表示会话可能正在被使用（有锁文件 / 属于当前进程），UI 会提示。
	Active bool `json:"active"`
}

// Kind 常量，和 catalog.Store.Kind 对齐。
const (
	StorageFile   = "file"
	StorageDir    = "dir"
	StorageSQLite = "sqlite"
)
