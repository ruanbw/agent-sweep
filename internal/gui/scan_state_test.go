package gui

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/scan"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// 这组测试只验证扫描状态机，**绝不跑真实扫描**：scan 包会扫开发机的真实目录，
// 既慢又有副作用。startScanWith 是同一文件内的小抽取，只为注入受控执行器，
// 生产路径仍然走 startScan → catalog.All + scan.AllWithProgress。

func scanStateService() *Service {
	return &Service{
		byUID: map[string]session.Session{},
		scan:  ScanStatus{Status: "idle", Issues: []ScanIssue{}},
	}
}

// waitForScanIdle 等到 scanning 复位，返回那一刻的状态快照。
// 超时直接 Fatal：状态机卡在 running 正是 R19 要防的回归。
func waitForScanIdle(t *testing.T, svc *Service) ScanStatus {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		svc.mu.Lock()
		scanning := svc.scanning
		status := svc.scan
		svc.mu.Unlock()
		if !scanning {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("扫描状态机没有复位：scanning 仍为 true，scan=%+v", status)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStartScanIsIdempotentWhileRunning(t *testing.T) {
	svc := scanStateService()
	var calls atomic.Int32
	entered := make(chan struct{}, 4)
	release := make(chan struct{})

	run := func(specs []catalog.Spec, progress func(done, total int, current string)) []scan.AgentResult {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return nil
	}
	specs := func() []catalog.Spec { return nil }

	svc.startScanWith(specs, run)
	<-entered

	// 第一次还在跑：第二次必须是空操作，不能并发再开一个。
	svc.startScanWith(specs, run)
	close(release)

	status := waitForScanIdle(t, svc)
	if got := calls.Load(); got != 1 {
		t.Fatalf("连续两次 startScan 应只启动一次扫描，实际执行了 %d 次", got)
	}
	if status.Status != "done" {
		t.Fatalf("正常路径终态应为 done，实际 %q：%+v", status.Status, status)
	}
}

func TestScanPanicConvergesToErrorAndAllowsRetry(t *testing.T) {
	svc := scanStateService()
	// 先放一份「上一次扫描」的数据：panic 路径不能拿半份结果覆盖它。
	previous := AgentView{AgentID: "old", Name: "Old"}
	svc.agents = []AgentView{previous}
	svc.sessions = []session.Session{{UID: "old:s1", AgentID: "old"}}

	specs := func() []catalog.Spec { return nil }
	svc.startScanWith(specs, func(_ []catalog.Spec, _ func(done, total int, current string)) []scan.AgentResult {
		panic("driver exploded")
	})

	status := waitForScanIdle(t, svc)
	if status.Status != "error" {
		t.Fatalf("panic 后终态必须是 error，实际 %q：%+v", status.Status, status)
	}
	if len(status.Issues) == 0 {
		t.Fatalf("panic 必须留下一条可读 issue，实际 %+v", status)
	}
	last := status.Issues[len(status.Issues)-1]
	if !strings.Contains(last.Message, "扫描异常终止") || !strings.Contains(last.Message, "driver exploded") {
		t.Fatalf("issue 应说明扫描异常终止及原因，实际 %+v", last)
	}
	if status.StartedAt == "" || status.FinishedAt == "" {
		t.Fatalf("终态必须带开始/结束时间，实际 %+v", status)
	}

	svc.mu.Lock()
	agents := append([]AgentView(nil), svc.agents...)
	sessions := append([]session.Session(nil), svc.sessions...)
	svc.mu.Unlock()
	if len(agents) != 1 || agents[0].AgentID != "old" || len(sessions) != 1 || sessions[0].UID != "old:s1" {
		t.Fatalf("panic 路径应保留上一次的扫描结果，实际 agents=%+v sessions=%+v", agents, sessions)
	}

	// 状态机必须已经复位：可以再次发起扫描，并且这次能正常走到 done。
	var calls atomic.Int32
	svc.startScanWith(specs, func(_ []catalog.Spec, _ func(done, total int, current string)) []scan.AgentResult {
		calls.Add(1)
		return nil
	})
	retried := waitForScanIdle(t, svc)
	if got := calls.Load(); got != 1 {
		t.Fatalf("panic 之后应能再次发起扫描，实际执行 %d 次", got)
	}
	if retried.Status != "done" {
		t.Fatalf("重试的终态应为 done，实际 %q：%+v", retried.Status, retried)
	}
}

func TestScanSuccessKeepsExistingSemantics(t *testing.T) {
	svc := scanStateService()
	spec := catalog.Spec{ID: "demo", Name: "Demo"}
	item := session.Session{UID: "demo:s1", AgentID: "demo", UpdatedAt: time.Now()}

	svc.startScanWith(
		func() []catalog.Spec { return []catalog.Spec{spec} },
		func(_ []catalog.Spec, progress func(done, total int, current string)) []scan.AgentResult {
			progress(1, 1, "Demo")
			return []scan.AgentResult{{
				AgentID:    "demo",
				Name:       "Demo",
				Sessions:   []session.Session{item},
				TotalBytes: 42,
				Err:        "有一个文件读不了",
			}}
		},
	)

	status := waitForScanIdle(t, svc)
	if status.Status != "done" {
		t.Fatalf("正常路径终态应为 done，实际 %q", status.Status)
	}
	if status.Progress.Done != 1 || status.Progress.Total != 1 {
		t.Fatalf("成功路径进度应为 1/1，实际 %+v", status.Progress)
	}
	if len(status.Issues) != 1 || status.Issues[0].AgentID != "demo" || status.Issues[0].Message != "有一个文件读不了" {
		t.Fatalf("Agent 的错误应原样进 Issues，实际 %+v", status.Issues)
	}

	svc.mu.Lock()
	defer svc.mu.Unlock()
	if len(svc.specs) != 1 || svc.specs[0].ID != "demo" {
		t.Fatalf("成功路径应整份替换 specs，实际 %+v", svc.specs)
	}
	if len(svc.agents) != 1 || svc.agents[0].SessionCount != 1 || svc.agents[0].TotalBytes != 42 {
		t.Fatalf("成功路径应整份替换 agents，实际 %+v", svc.agents)
	}
	if len(svc.sessions) != 1 || svc.sessions[0].UID != "demo:s1" {
		t.Fatalf("成功路径应整份替换 sessions，实际 %+v", svc.sessions)
	}
	if _, ok := svc.byUID["demo:s1"]; !ok {
		t.Fatalf("成功路径应重建 byUID，实际 %+v", svc.byUID)
	}
}
