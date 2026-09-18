package sanvm

import (
	"math/big"
	"strings"
	"testing"
)

// TestCollectionItemCapRejectsCleanly pins the Go-only collection cap: a list
// cannot grow past the configured item count and the VM fails with a VMError.
func TestCollectionItemCapRejectsCleanly(t *testing.T) {
	virtualMachine := NewVM(NewStorage())
	virtualMachine.Verbose = false
	virtualMachine.MaxCollectionItems = 4
	virtualMachine.Storage.SetVar("items", []any{int64(1), int64(2), int64(3), int64(4)})

	err := virtualMachine.Execute([]any{
		int(OpPush), "items",
		int(OpPush), int64(5),
		int(OpListAppend),
		int(OpHalt),
	})
	if err == nil {
		t.Fatalf("list append past the cap was accepted")
	}
	if _, ok := err.(*VMError); !ok {
		t.Fatalf("error type %T, want *VMError", err)
	}
	if !strings.Contains(err.Error(), "exceeds 4 items") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(virtualMachine.Storage.GetVar("items").([]any)); got != 4 {
		t.Fatalf("list grew to %d items despite the cap", got)
	}
}

// TestCollectionItemCapAllowsGrowthBelowLimit verifies the cap does not fire
// for legitimate appends.
func TestCollectionItemCapAllowsGrowthBelowLimit(t *testing.T) {
	virtualMachine := NewVM(NewStorage())
	virtualMachine.Verbose = false
	virtualMachine.Storage.SetVar("items", []any{})
	if err := virtualMachine.Execute([]any{
		int(OpPush), "items",
		int(OpPush), int64(1),
		int(OpListAppend),
		int(OpHalt),
	}); err != nil {
		t.Fatalf("append below the cap failed: %v", err)
	}
	if got := len(virtualMachine.Storage.GetVar("items").([]any)); got != 1 {
		t.Fatalf("list length %d, want 1", got)
	}
}

// TestDictListGrowthCapRejectsCleanly covers the list-backed dict growth path.
func TestDictListGrowthCapRejectsCleanly(t *testing.T) {
	virtualMachine := NewVM(NewStorage())
	virtualMachine.Verbose = false
	virtualMachine.MaxCollectionItems = 2
	virtualMachine.Storage.SetVar("m", []any{int64(1), int64(2)})

	err := virtualMachine.Execute([]any{
		int(OpPush), "m",
		int(OpPush), int64(2),
		int(OpPush), int64(3),
		int(OpDictSet),
		int(OpHalt),
	})
	if err == nil {
		t.Fatalf("dict list growth past the cap was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds 2 items") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestListRemoveScanIsPricedForLargeLists asserts the superlinear guard: a
// large list removal costs more gas than a small one, while both stay bounded.
func TestListRemoveScanIsPricedForLargeLists(t *testing.T) {
	run := func(size int) int64 {
		virtualMachine := NewVM(NewStorage())
		virtualMachine.Verbose = false
		items := make([]any, size)
		for index := range items {
			items[index] = int64(index)
		}
		virtualMachine.Storage.SetVar("items", items)
		if err := virtualMachine.Execute([]any{
			int(OpPush), "items",
			int(OpPush), int64(size + 1),
			int(OpListRemove),
			int(OpHalt),
		}); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		return virtualMachine.GasUsed
	}
	small := run(100)
	large := run(10_000)
	if large <= small {
		t.Fatalf("large-list removal gas %d did not exceed small-list gas %d", large, small)
	}
	if large > small+100 {
		t.Fatalf("large-list removal gas grew unexpectedly: small=%d large=%d", small, large)
	}
}

// TestDictKeysSortIsPricedForLargeDicts is the DICT_KEYS analogue.
func TestDictKeysSortIsPricedForLargeDicts(t *testing.T) {
	run := func(size int) int64 {
		virtualMachine := NewVM(NewStorage())
		virtualMachine.Verbose = false
		dictionary := map[string]any{}
		for index := 0; index < size; index++ {
			dictionary[string(rune('a'+index%26))+string(rune('a'+index/26))] = int64(index)
		}
		virtualMachine.Storage.SetVar("m", dictionary)
		if err := virtualMachine.Execute([]any{
			int(OpPush), "m",
			int(OpDictKeys),
			int(OpHalt),
		}); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		return virtualMachine.GasUsed
	}
	small := run(100)
	large := run(5_000)
	if large <= small {
		t.Fatalf("large-dict DICT_KEYS gas %d did not exceed small-dict gas %d", large, small)
	}
}

// TestValueAndIntegerLimits pins the stack value caps: serialized containers
// above MaxValueBytes and integers above MaxIntBits are rejected with a
// VMError, never truncated.
func TestValueAndIntegerLimits(t *testing.T) {
	oversized := strings.Repeat("a", MaxValueBytes+1)
	virtualMachine := NewVM(NewStorage())
	virtualMachine.Verbose = false
	if err := virtualMachine.Execute([]any{
		int(OpPush), oversized,
		int(OpHalt),
	}); err == nil || !strings.Contains(err.Error(), "Value exceeds") {
		t.Fatalf("oversized string error = %v, want Value exceeds", err)
	}

	huge := new(big.Int).Lsh(big.NewInt(1), MaxIntBits+1)
	virtualMachine = NewVM(NewStorage())
	virtualMachine.Verbose = false
	if err := virtualMachine.Execute([]any{
		int(OpPush), huge,
		int(OpHalt),
	}); err == nil || !strings.Contains(err.Error(), "Integer exceeds") {
		t.Fatalf("oversized integer error = %v, want Integer exceeds", err)
	}
}

// TestHostileCollectionsNeverPanic drives the capped collection paths with
// hostile inputs and asserts clean errors instead of panics.
func TestHostileCollectionsNeverPanic(t *testing.T) {
	cases := [][]any{
		{int(OpPush), "missing", int(OpPush), int64(1), int(OpListAppend)},
		{int(OpPush), int64(1), int(OpPush), int64(2), int(OpListRemove)},
		{int(OpPush), "x", int(OpPush), -1, int(OpListGet)},
		{int(OpPush), "x", int(OpPush), int64(1), int(OpDictGet)},
		{int(OpPush), "x", int(OpDictKeys)},
		{int(OpPush), int64(1), int(OpDictKeys)},
	}
	for index, bytecode := range cases {
		guardNoPanic(t, "hostile collection", func() {
			virtualMachine := NewVM(NewStorage())
			virtualMachine.Verbose = false
			_ = virtualMachine.Execute(bytecode)
		})
		_ = index
	}
}
