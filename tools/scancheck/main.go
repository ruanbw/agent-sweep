// scancheck 是一个诊断工具：把当前目录目录里每个 Agent 扫出来的前几条会话打出来。
//
// 它存在的理由：UI 上只看到「0 条」时，问题可能出在路径、在解析、在权限，
// 需要一个不依赖 WindowServer 的东西把中间结果摊开看。
//
//	go run ./tools/scancheck [agentId]
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/scan"
)

func main() {
	filter := ""
	if len(os.Args) > 1 {
		filter = os.Args[1]
	}
	started := time.Now()
	results := scan.All(catalog.All())
	fmt.Printf("扫描 %d 个 Agent，耗时 %s\n\n", len(results), time.Since(started).Round(time.Millisecond))

	for _, result := range results {
		if filter != "" && result.AgentID != filter {
			continue
		}
		if !result.Installed {
			continue
		}
		fmt.Printf("■ %s (%s)  %d 条  %d B  %s\n", result.Name, result.AgentID, len(result.Sessions), result.TotalBytes, result.Home)
		if result.Err != "" {
			fmt.Printf("  ! %s\n", result.Err)
		}
		for i, item := range result.Sessions {
			if i >= 3 {
				fmt.Printf("  … 还有 %d 条\n", len(result.Sessions)-3)
				break
			}
			active := ""
			if item.Active {
				active = "  [使用中]"
			}
			fmt.Printf("  - %-38.38s %s  %s%s\n", item.Title, item.UpdatedAt.Format("01-02 15:04"), item.Project, active)
			for _, path := range item.Paths {
				fmt.Printf("      %s\n", path)
			}
		}
	}
}
