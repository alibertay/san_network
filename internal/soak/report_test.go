package soak

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func healthyViews() []NodeView {
	return []NodeView{
		{
			Name: "N0", Height: 20, StartHeight: 1, FinalizedHeight: 15,
			StateRoot: "root-a", TipHash: "hash-a", Mempool: 0,
			Validators: []string{"v1", "v2"}, TotalStake: 200,
			KnownBalance: 1200, KnownStake: 200, TotalBurned: 5, TotalSlashed: 0,
			GenesisTotal: 1365, BlockReward: 2, MoneyCheck: true,
			Goroutines: 10, MemoryAlloc: 1024,
		},
		{
			Name: "N1", Height: 21, StartHeight: 1, FinalizedHeight: 16,
			StateRoot: "root-b", TipHash: "hash-b", Mempool: 1,
			Validators: []string{"v2", "v1"}, TotalStake: 200,
			KnownBalance: 1200, KnownStake: 200, TotalBurned: 5, TotalSlashed: 0,
			GenesisTotal: 1363, BlockReward: 2, MoneyCheck: true,
			Goroutines: 11, MemoryAlloc: 1100,
		},
	}
}

func findCheck(t *testing.T, checks []CheckResult, name string) CheckResult {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("check %s not found in %v", name, checks)
	return CheckResult{}
}

func TestEvaluateHealthyViews(t *testing.T) {
	checks := Evaluate(healthyViews(), DefaultOptions())
	for _, check := range checks {
		if !check.Passed {
			t.Errorf("healthy views should pass %s: %s", check.Name, check.Detail)
		}
	}
}

func TestEvaluateFailsOnHeightDivergence(t *testing.T) {
	views := healthyViews()
	views[1].Height = 40
	views[1].StateRoot = "root-b"
	checks := Evaluate(views, DefaultOptions())
	if check := findCheck(t, checks, "chain_convergence"); check.Passed {
		t.Errorf("chain_convergence should fail: %s", check.Detail)
	}
}

func TestEvaluateFailsOnStateRootMismatch(t *testing.T) {
	views := healthyViews()
	views[0].Height = 21
	views[1].Height = 21
	views[0].StateRoot = "root-x"
	views[1].StateRoot = "root-y"
	views[1].FinalizedHeight = views[0].FinalizedHeight
	checks := Evaluate(views, DefaultOptions())
	if check := findCheck(t, checks, "state_root_equality"); check.Passed {
		t.Errorf("state_root_equality should fail: %s", check.Detail)
	}
}

func TestEvaluateFailsOnMoneyMismatch(t *testing.T) {
	views := healthyViews()
	views[0].KnownBalance = 999
	checks := Evaluate(views, DefaultOptions())
	if check := findCheck(t, checks, "money_conservation"); check.Passed {
		t.Errorf("money_conservation should fail: %s", check.Detail)
	}
}

func TestEvaluateFailsOnValidatorSetMismatch(t *testing.T) {
	views := healthyViews()
	views[1].Validators = []string{"v3"}
	checks := Evaluate(views, DefaultOptions())
	if check := findCheck(t, checks, "validator_set_equality"); check.Passed {
		t.Errorf("validator_set_equality should fail: %s", check.Detail)
	}
}

func TestEvaluateFailsOnStuckNode(t *testing.T) {
	views := healthyViews()
	views[1].StartHeight = views[1].Height
	checks := Evaluate(views, DefaultOptions())
	if check := findCheck(t, checks, "no_stuck_nodes"); check.Passed {
		t.Errorf("no_stuck_nodes should fail: %s", check.Detail)
	}
}

func TestEvaluateFailsOnUnreachableNode(t *testing.T) {
	views := healthyViews()
	views[0].Error = "connection refused"
	checks := Evaluate(views, DefaultOptions())
	if check := findCheck(t, checks, "nodes_reachable"); check.Passed {
		t.Errorf("nodes_reachable should fail: %s", check.Detail)
	}
}

func TestEvaluateFailsOnMempoolOverflow(t *testing.T) {
	views := healthyViews()
	views[0].Mempool = 9000
	checks := Evaluate(views, DefaultOptions())
	if check := findCheck(t, checks, "mempool_bounded"); check.Passed {
		t.Errorf("mempool_bounded should fail: %s", check.Detail)
	}
}

func TestCheckGrowth(t *testing.T) {
	check := CheckGrowth([]GrowthPoint{
		{Name: "goroutines", Start: 10, End: 12, MaxRatio: 4},
		{Name: "memory", Start: 1000, End: 2000, MaxRatio: 6},
	})
	if !check.Passed {
		t.Fatalf("bounded growth should pass: %s", check.Detail)
	}
	check = CheckGrowth([]GrowthPoint{{Name: "memory", Start: 1000, End: 50000, MaxRatio: 6}})
	if check.Passed {
		t.Fatalf("unbounded growth should fail")
	}
}

func TestReportRenderAndPass(t *testing.T) {
	checks := Evaluate(healthyViews(), DefaultOptions())
	report := Build(90*time.Second, 2, 5, 42, checks)
	if !report.Pass() {
		t.Fatalf("healthy report should pass: %+v", report)
	}
	var buffer bytes.Buffer
	report.Render(&buffer)
	text := buffer.String()
	for _, expected := range []string{"PASS money_conservation", "PASS state_root_equality", "PASS: 2 node(s)", "42 transaction(s)"} {
		if !strings.Contains(text, expected) {
			t.Errorf("report missing %q:\n%s", expected, text)
		}
	}
	encoded, err := report.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if !strings.Contains(string(encoded), `"failures": 0`) {
		t.Errorf("JSON missing failures: %s", encoded)
	}
}

func TestReportFails(t *testing.T) {
	views := healthyViews()
	views[0].Height = 1
	views[0].StartHeight = 1
	report := Build(time.Second, 2, 1, 0, Evaluate(views, DefaultOptions()))
	if report.Pass() {
		t.Fatal("report with a stuck node should fail")
	}
	var buffer bytes.Buffer
	report.Render(&buffer)
	if !strings.Contains(buffer.String(), "FAIL:") {
		t.Errorf("verdict should be FAIL:\n%s", buffer.String())
	}
}
