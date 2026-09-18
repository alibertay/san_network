// Package soak contains the deterministic report logic of cmd/sansoak. It is
// separated from the network harness so the invariants can be unit-tested
// with synthetic node views and no real VPS time.
package soak

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// NodeView is one sample of a node's externally visible state.
type NodeView struct {
	Name            string   `json:"name"`
	Height          int64    `json:"height"`
	StartHeight     int64    `json:"start_height"`
	FinalizedHeight int64    `json:"finalized_height"`
	StateRoot       string   `json:"state_root"`
	TipHash         string   `json:"tip_hash"`
	Mempool         int64    `json:"mempool"`
	Validators      []string `json:"validators"`
	TotalStake      int64    `json:"total_stake"`
	// Money conservation inputs (balance of every known wallet plus its
	// stake). MoneyCheck reports whether they are meaningful for this view.
	KnownBalance int64  `json:"known_balance"`
	KnownStake   int64  `json:"known_stake"`
	TotalBurned  int64  `json:"total_burned"`
	TotalSlashed int64  `json:"total_slashed"`
	GenesisTotal int64  `json:"genesis_total"`
	BlockReward  int64  `json:"block_reward"`
	MoneyCheck   bool   `json:"money_check"`
	Goroutines   int64  `json:"goroutines"`
	MemoryAlloc  int64  `json:"memory_alloc"`
	Error        string `json:"error"`
}

// Options tunes the invariant thresholds.
type Options struct {
	HeightTolerance    int64
	FinalityTolerance  int64
	MaxMempool         int64
	MaxGoroutineGrowth float64 // ratio, e.g. 3.0 means 3x
	MaxMemoryGrowth    float64
	MinHeightProgress  int64
}

// DefaultOptions returns the devnet-friendly thresholds.
func DefaultOptions() Options {
	return Options{
		HeightTolerance:    3,
		FinalityTolerance:  10,
		MaxMempool:         2048,
		MaxGoroutineGrowth: 4.0,
		MaxMemoryGrowth:    6.0,
		MinHeightProgress:  1,
	}
}

// CheckResult is one invariant verdict.
type CheckResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// Report is the final soak verdict.
type Report struct {
	GeneratedAt     time.Time     `json:"generated_at"`
	DurationSeconds float64       `json:"duration_seconds"`
	Nodes           int           `json:"nodes"`
	Cycles          int64         `json:"cycles"`
	Transactions    int64         `json:"transactions_submitted"`
	Checks          []CheckResult `json:"checks"`
	Failures        int           `json:"failures"`
}

// Pass reports whether every check passed.
func (report Report) Pass() bool { return report.Failures == 0 }

// Evaluate runs every invariant over the end-of-run node views.
func Evaluate(views []NodeView, opts Options) []CheckResult {
	checks := []CheckResult{}
	if len(views) == 0 {
		return append(checks, CheckResult{Name: "nodes_present", Passed: false, Detail: "no node samples collected"})
	}

	checks = append(checks, checkReachable(views))
	if opts.MinHeightProgress > 0 {
		checks = append(checks, checkProgress(views, opts.MinHeightProgress))
	}
	checks = append(checks, checkConvergence(views, opts.HeightTolerance, "chain_convergence", "height"))
	checks = append(checks, checkConvergence(views, opts.FinalityTolerance, "finality_convergence", "finalized height"))
	checks = append(checks, checkStateRoots(views))
	checks = append(checks, checkValidatorSets(views))
	checks = append(checks, checkMempool(views, opts.MaxMempool))
	checks = append(checks, checkMoney(views))
	return checks
}

func checkReachable(views []NodeView) CheckResult {
	unreachable := []string{}
	for _, view := range views {
		if strings.TrimSpace(view.Error) != "" {
			unreachable = append(unreachable, view.Name+": "+view.Error)
		}
	}
	sort.Strings(unreachable)
	if len(unreachable) > 0 {
		return CheckResult{Name: "nodes_reachable", Passed: false, Detail: strings.Join(unreachable, "; ")}
	}
	return CheckResult{Name: "nodes_reachable", Passed: true}
}

func checkProgress(views []NodeView, minimum int64) CheckResult {
	stuck := []string{}
	for _, view := range views {
		if view.Error != "" {
			continue
		}
		if view.Height-view.StartHeight < minimum {
			stuck = append(stuck, fmt.Sprintf("%s advanced %d block(s)", view.Name, view.Height-view.StartHeight))
		}
	}
	sort.Strings(stuck)
	if len(stuck) > 0 {
		return CheckResult{Name: "no_stuck_nodes", Passed: false, Detail: strings.Join(stuck, "; ")}
	}
	return CheckResult{Name: "no_stuck_nodes", Passed: true}
}

// checkConvergence verifies that a numeric gauge (height or finalized height)
// stays within tolerance across all live nodes.
func checkConvergence(views []NodeView, tolerance int64, name, label string) CheckResult {
	values := []int64{}
	for _, view := range views {
		if view.Error != "" {
			continue
		}
		if label == "height" {
			values = append(values, view.Height)
		} else {
			values = append(values, view.FinalizedHeight)
		}
	}
	if len(values) < 2 {
		return CheckResult{Name: name, Passed: true, Detail: "fewer than two live nodes"}
	}
	minimum, maximum := values[0], values[0]
	for _, value := range values {
		if value < minimum {
			minimum = value
		}
		if value > maximum {
			maximum = value
		}
	}
	if maximum-minimum > tolerance {
		return CheckResult{
			Name:   name,
			Passed: false,
			Detail: fmt.Sprintf("%s spread %d exceeds tolerance %d", label, maximum-minimum, tolerance),
		}
	}
	return CheckResult{Name: name, Passed: true, Detail: fmt.Sprintf("%s spread %d", label, maximum-minimum)}
}

// checkStateRoots requires nodes at the same height to agree on the state
// root and tip hash.
func checkStateRoots(views []NodeView) CheckResult {
	type rootKey struct {
		height int64
		root   string
		hash   string
	}
	byHeight := map[int64][]NodeView{}
	for _, view := range views {
		if view.Error != "" {
			continue
		}
		byHeight[view.Height] = append(byHeight[view.Height], view)
	}
	for height, group := range byHeight {
		if len(group) < 2 {
			continue
		}
		referenceRoot := group[0].StateRoot
		referenceHash := group[0].TipHash
		for _, view := range group[1:] {
			if view.StateRoot != referenceRoot {
				return CheckResult{
					Name:   "state_root_equality",
					Passed: false,
					Detail: fmt.Sprintf("height %d: %s root %s != %s root %s",
						height, group[0].Name, shortHash(referenceRoot), view.Name, shortHash(view.StateRoot)),
				}
			}
			if referenceHash != "" && view.TipHash != "" && view.TipHash != referenceHash {
				return CheckResult{
					Name:   "chain_convergence",
					Passed: false,
					Detail: fmt.Sprintf("height %d: %s tip %s != %s tip %s",
						height, group[0].Name, shortHash(referenceHash), view.Name, shortHash(view.TipHash)),
				}
			}
		}
		_ = rootKey{height: height, root: referenceRoot, hash: referenceHash}
	}
	return CheckResult{Name: "state_root_equality", Passed: true}
}

// checkValidatorSets requires every live node to expose the same active
// validator set once chains have converged.
func checkValidatorSets(views []NodeView) CheckResult {
	var reference []string
	referenceName := ""
	for _, view := range views {
		if view.Error != "" {
			continue
		}
		set := append([]string{}, view.Validators...)
		sort.Strings(set)
		if reference == nil {
			reference = set
			referenceName = view.Name
			continue
		}
		if !equalStrings(reference, set) {
			return CheckResult{
				Name:   "validator_set_equality",
				Passed: false,
				Detail: fmt.Sprintf("%s has %v, %s has %v", referenceName, reference, view.Name, set),
			}
		}
	}
	return CheckResult{Name: "validator_set_equality", Passed: true}
}

func checkMempool(views []NodeView, maximum int64) CheckResult {
	over := []string{}
	for _, view := range views {
		if view.Error != "" {
			continue
		}
		if maximum > 0 && view.Mempool > maximum {
			over = append(over, fmt.Sprintf("%s mempool %d > %d", view.Name, view.Mempool, maximum))
		}
	}
	sort.Strings(over)
	if len(over) > 0 {
		return CheckResult{Name: "mempool_bounded", Passed: false, Detail: strings.Join(over, "; ")}
	}
	return CheckResult{Name: "mempool_bounded", Passed: true}
}

// checkMoney validates the conservation equation:
//
//	known balances + known stakes + burned + slashed
//	  == genesis total + block_reward * height
func checkMoney(views []NodeView) CheckResult {
	failures := []string{}
	for _, view := range views {
		if view.Error != "" || !view.MoneyCheck {
			continue
		}
		expected := view.GenesisTotal + view.BlockReward*view.Height
		actual := view.KnownBalance + view.KnownStake + view.TotalBurned + view.TotalSlashed
		if actual != expected {
			failures = append(failures, fmt.Sprintf("%s: supply %d != expected %d (delta %d)",
				view.Name, actual, expected, actual-expected))
		}
	}
	sort.Strings(failures)
	if len(failures) > 0 {
		return CheckResult{Name: "money_conservation", Passed: false, Detail: strings.Join(failures, "; ")}
	}
	return CheckResult{Name: "money_conservation", Passed: true}
}

// GrowthCheck compares two samples of the same gauge.
type GrowthPoint struct {
	Name     string  `json:"name"`
	Start    float64 `json:"start"`
	End      float64 `json:"end"`
	MaxRatio float64 `json:"max_ratio"`
}

// CheckGrowth evaluates resource growth across the whole run.
func CheckGrowth(points []GrowthPoint) CheckResult {
	failures := []string{}
	for _, point := range points {
		if point.Start <= 0 || point.End <= 0 {
			continue
		}
		ratio := point.End / point.Start
		if point.MaxRatio > 0 && ratio > point.MaxRatio {
			failures = append(failures, fmt.Sprintf("%s grew %.2fx (%.0f -> %.0f)", point.Name, ratio, point.Start, point.End))
		}
	}
	sort.Strings(failures)
	if len(failures) > 0 {
		return CheckResult{Name: "resource_growth_bounded", Passed: false, Detail: strings.Join(failures, "; ")}
	}
	return CheckResult{Name: "resource_growth_bounded", Passed: true}
}

// Build assembles the final report and counts failures.
func Build(duration time.Duration, nodes int, cycles, transactions int64, checks []CheckResult) Report {
	failures := 0
	for _, check := range checks {
		if !check.Passed {
			failures++
		}
	}
	return Report{
		GeneratedAt:     time.Now().UTC(),
		DurationSeconds: duration.Seconds(),
		Nodes:           nodes,
		Cycles:          cycles,
		Transactions:    transactions,
		Checks:          checks,
		Failures:        failures,
	}
}

// Render prints the human PASS/FAIL report.
func (report Report) Render(w io.Writer) {
	fmt.Fprintf(w, "\n=== sansoak report ===\n")
	for _, check := range report.Checks {
		status := "PASS"
		if !check.Passed {
			status = "FAIL"
		}
		detail := ""
		if check.Detail != "" {
			detail = " - " + check.Detail
		}
		fmt.Fprintf(w, "%s %s%s\n", status, check.Name, detail)
	}
	verdict := "PASS"
	if !report.Pass() {
		verdict = "FAIL"
	}
	fmt.Fprintf(w, "%s: %d node(s), %.1fs, %d cycle(s), %d transaction(s), %d failure(s)\n",
		verdict, report.Nodes, report.DurationSeconds, report.Cycles, report.Transactions, report.Failures)
}

// JSON renders the report as machine-readable JSON.
func (report Report) JSON() ([]byte, error) {
	return json.MarshalIndent(report, "", "  ")
}

// ---------------------------------------------------------------------- #
// helpers
// ---------------------------------------------------------------------- #

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// Ratio is a small helper for growth math (exported for tests).
func Ratio(start, end float64) float64 {
	if start <= 0 {
		return math.Inf(1)
	}
	return end / start
}
