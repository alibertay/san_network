package bench

import (
	"testing"
)

// assertResults checks that every result completed. Rate values may round to
// zero on hosts with a coarse wall clock (short smoke iterations), so only
// errors and structural fields are asserted here.
func assertResults(t *testing.T, results []Result, requirePositive bool) {
	t.Helper()
	if len(results) == 0 {
		t.Fatal("no results")
	}
	for _, result := range results {
		if result.Error != "" {
			t.Errorf("%s: %s", result.Name, result.Error)
		}
	}
}

func TestSignaturesSmoke(t *testing.T) {
	results := Signatures(2)
	assertResults(t, results, true)
	names := map[string]bool{}
	for _, result := range results {
		names[result.Name] = true
	}
	for _, expected := range []string{"mldsa44.sign", "mldsa44.verify", "mldsa44.signature_size"} {
		if !names[expected] {
			t.Errorf("missing result %s", expected)
		}
	}
}

func TestTransactionsSmoke(t *testing.T) {
	results := Transactions(5)
	assertResults(t, results, true)
	names := map[string]bool{}
	for _, result := range results {
		names[result.Name] = true
	}
	for _, expected := range []string{"transaction.validation", "transaction.serialize", "transaction.average_bytes"} {
		if !names[expected] {
			t.Errorf("missing result %s", expected)
		}
	}
}

func TestVMSmoke(t *testing.T) {
	results := VM(3)
	assertResults(t, results, true)
	names := map[string]bool{}
	for _, result := range results {
		names[result.Name] = true
	}
	for _, expected := range []string{"simple_transfer", "storage_heavy", "arithmetic_heavy", "sanrc20_transfer", "amm_swap"} {
		if !names[expected] {
			t.Errorf("missing workload %s (results: %v)", expected, results)
		}
	}
}

func TestSizesSmoke(t *testing.T) {
	results := Sizes()
	if len(results) < 10 {
		t.Fatalf("expected at least 10 size results, got %d", len(results))
	}
	for _, result := range results {
		if result.Error != "" {
			t.Errorf("%s: %s", result.Name, result.Error)
			continue
		}
		if result.Bytes <= 0 {
			t.Errorf("%s: non-positive size %d", result.Name, result.Bytes)
		}
	}
}

func TestBlockValidationSmoke(t *testing.T) {
	results := BlockValidation([]int{2, 5})
	assertResults(t, results, true)
	for _, result := range results {
		if result.Extra["transactions"] == nil {
			t.Errorf("%s: missing transactions detail", result.Name)
		}
	}
}
