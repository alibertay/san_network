package parity_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/sanvm"
)

func TestSANVMPrograms(t *testing.T) {
	root := fixture(t)
	programs, ok := root["sanvm_programs"].([]any)
	if !ok {
		t.Fatalf("sanvm_programs fixture missing")
	}
	for _, raw := range programs {
		entry := raw.(map[string]any)
		name := mustString(t, entry["name"])
		language := mustString(t, entry["language"])
		source := mustString(t, entry["source"])

		bytecode, err := sanvm.CompilePena(source, language)
		if err != nil {
			t.Fatalf("%s: compile: %v", name, err)
		}
		if got, want := canonicalString(t, bytecode), mustString(t, entry["bytecode_json"]); got != want {
			t.Errorf("%s: bytecode mismatch:\n got %s\nwant %s", name, got, want)
		}

		decoded, err := canonical.Decode([]byte(mustString(t, entry["bytecode_json"])))
		if err != nil {
			t.Fatalf("%s: decode bytecode: %v", name, err)
		}
		virtualMachine := sanvm.NewVM(sanvm.NewStorage())
		virtualMachine.Verbose = false
		if err := virtualMachine.Execute(decoded.([]any)); err != nil {
			t.Fatalf("%s: run: %v", name, err)
		}
		if got, want := canonicalString(t, virtualMachine.Logs), mustString(t, entry["logs"]); got != want {
			t.Errorf("%s: logs:\n got %s\nwant %s", name, got, want)
		}
		if got, want := canonicalString(t, virtualMachine.Stack), mustString(t, entry["stack"]); got != want {
			t.Errorf("%s: stack:\n got %s\nwant %s", name, got, want)
		}
		if want := entry["gas_used"].(int64); virtualMachine.GasUsed != want {
			t.Errorf("%s: gas used: got %d, want %d", name, virtualMachine.GasUsed, want)
		}
	}
}

func TestSANVMContracts(t *testing.T) {
	root := fixture(t)
	section := root["sanvm_contracts"].(map[string]any)

	tokenSource := mustString(t, section["token_source"])
	boomSource := mustString(t, section["boom_source"])

	virtualMachine := sanvm.NewVM(sanvm.NewStorage())
	virtualMachine.Verbose = false

	tokenBytecode, err := sanvm.NewPenaParser().Parse(tokenSource)
	if err != nil {
		t.Fatalf("token compile: %v", err)
	}
	if err := virtualMachine.DeployContract("token", tokenBytecode, nil); err != nil {
		t.Fatalf("token deploy: %v", err)
	}
	setResult, err := virtualMachine.CallContractFunction("token", "set", []any{"alice", int64(100)}, nil)
	if err != nil {
		t.Fatalf("set call: %v", err)
	}
	if setResult != nil {
		t.Errorf("set result: got %v, want nil", setResult)
	}
	if got, want := canonicalString(t, virtualMachine.ContractManager.LastLogs),
		mustString(t, section["set_logs"]); got != want {
		t.Errorf("set logs:\n got %s\nwant %s", got, want)
	}
	if want := section["set_gas"].(int64); virtualMachine.ContractManager.LastGasUsed != want {
		t.Errorf("set gas: got %d, want %d", virtualMachine.ContractManager.LastGasUsed, want)
	}
	getResult, err := virtualMachine.CallContractFunction("token", "get", []any{"alice"}, nil)
	if err != nil {
		t.Fatalf("get call: %v", err)
	}
	if got, want := getResult, section["get_result"]; toI64Any(got) != toI64Any(want) {
		t.Errorf("get result: got %v, want %v", got, want)
	}
	contract := virtualMachine.Storage.Contracts["token"]
	if got, want := canonicalString(t, contract["storage"]), mustString(t, section["token_storage"]); got != want {
		t.Errorf("token storage:\n got %s\nwant %s", got, want)
	}

	boomBytecode, err := sanvm.NewPenaParser().Parse(boomSource)
	if err != nil {
		t.Fatalf("boom compile: %v", err)
	}
	if err := virtualMachine.DeployContract("boom", boomBytecode, nil); err != nil {
		t.Fatalf("boom deploy: %v", err)
	}
	_, err = virtualMachine.CallContractFunction("boom", "fail", []any{}, nil)
	if err == nil {
		t.Fatalf("failing call should return an error")
	}
	if want := mustString(t, section["failure"]); want != "VMError" {
		t.Fatalf("unexpected fixture failure type %q", want)
	}
	var vmErr *sanvm.VMError
	if !errors.As(err, &vmErr) {
		t.Errorf("failure type: got %T, want *sanvm.VMError", err)
	}
	rollbackResult, err := virtualMachine.CallContractFunction("boom", "get", []any{}, nil)
	if err != nil {
		t.Fatalf("rollback get: %v", err)
	}
	if got, want := toI64Any(rollbackResult), toI64Any(section["rollback_result"]); got != want {
		t.Errorf("rollback get: got %v, want %v", rollbackResult, section["rollback_result"])
	}

	if err := virtualMachine.DeployContract("token", tokenBytecode, nil); err == nil {
		t.Errorf("duplicate deploy should fail")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate deploy error: %v", err)
	}
}

func TestSANVMLimits(t *testing.T) {
	root := fixture(t)
	cases, ok := root["sanvm_limits"].([]any)
	if !ok {
		t.Fatalf("sanvm_limits fixture missing")
	}
	for _, raw := range cases {
		entry := raw.(map[string]any)
		name := mustString(t, entry["case"])
		want := mustString(t, entry["error"])

		var err error
		switch name {
		case "steps":
			source := "while (1) { }"
			bytecode, compileErr := sanvm.NewPenaParser().Parse(source)
			if compileErr != nil {
				t.Fatalf("steps compile: %v", compileErr)
			}
			virtualMachine := sanvm.NewVM(sanvm.NewStorage())
			virtualMachine.SetMaxSteps(500)
			err = virtualMachine.Execute(bytecode)
		case "stack":
			bytecode := []any{}
			for i := 0; i < 20; i++ {
				bytecode = append(bytecode, int(sanvm.OpPush), int64(1))
			}
			virtualMachine := sanvm.NewVM(sanvm.NewStorage())
			virtualMachine.SetMaxStack(8)
			err = virtualMachine.Execute(bytecode)
		case "gas":
			source := "x = 1\nwhile (x < 1000) {\n x = x + 1\n}"
			bytecode, compileErr := sanvm.NewPenaParser().Parse(source)
			if compileErr != nil {
				t.Fatalf("gas compile: %v", compileErr)
			}
			virtualMachine := sanvm.NewVM(sanvm.NewStorage())
			limit := int64(10)
			virtualMachine.SetGasLimit(&limit)
			err = virtualMachine.Execute(bytecode)
		case "divzero":
			source := "x = 1 / 0"
			bytecode, compileErr := sanvm.NewPenaParser().Parse(source)
			if compileErr != nil {
				t.Fatalf("divzero compile: %v", compileErr)
			}
			virtualMachine := sanvm.NewVM(sanvm.NewStorage())
			err = virtualMachine.Execute(bytecode)
		default:
			t.Fatalf("unknown limit case %q", name)
		}

		if err == nil {
			t.Errorf("%s: expected %s, got no error", name, want)
			continue
		}
		switch want {
		case "StepLimitExceeded":
			var target *sanvm.StepLimitExceeded
			if !errors.As(err, &target) {
				t.Errorf("%s: got %T (%v), want StepLimitExceeded", name, err, err)
			}
		case "StackLimitExceeded":
			var target *sanvm.StackLimitExceeded
			if !errors.As(err, &target) {
				t.Errorf("%s: got %T (%v), want StackLimitExceeded", name, err, err)
			}
		case "OutOfGas":
			var target *sanvm.OutOfGas
			if !errors.As(err, &target) {
				t.Errorf("%s: got %T (%v), want OutOfGas", name, err, err)
			}
		case "VMError":
			var target *sanvm.VMError
			if !errors.As(err, &target) {
				t.Errorf("%s: got %T (%v), want VMError", name, err, err)
			}
		}
	}
}

func TestSANVMCompileErrors(t *testing.T) {
	root := fixture(t)
	cases, ok := root["sanvm_errors"].([]any)
	if !ok {
		t.Fatalf("sanvm_errors fixture missing")
	}
	for index, raw := range cases {
		entry := raw.(map[string]any)
		kind := mustString(t, entry["kind"])
		source := mustString(t, entry["source"])
		wantError := entry["error"] != nil
		var err error
		if kind == "asm" {
			_, err = sanvm.Assemble(source)
		} else {
			_, err = sanvm.NewPenaParser().Parse(source)
		}
		if wantError && err == nil {
			t.Errorf("errors[%d] (%s): expected error for %q", index, kind, source)
		}
		if !wantError && err != nil {
			t.Errorf("errors[%d] (%s): unexpected error for %q: %v", index, kind, source, err)
		}
	}
}

func TestSANVMASM(t *testing.T) {
	root := fixture(t)
	entries, ok := root["sanvm_asm"].([]any)
	if !ok {
		t.Fatalf("sanvm_asm fixture missing")
	}
	for index, raw := range entries {
		entry := raw.(map[string]any)
		source := mustString(t, entry["source"])
		wantBytecode := mustString(t, entry["bytecode_json"])
		disassembly := mustString(t, entry["disassembly"])

		bytecode, err := sanvm.Assemble(source)
		if err != nil {
			t.Fatalf("asm[%d]: %v", index, err)
		}
		if got := canonicalString(t, bytecode); got != wantBytecode {
			t.Errorf("asm[%d] bytecode:\n got %s\nwant %s", index, got, wantBytecode)
		}

		fromPython, err := sanvm.AssembleLines(strings.Split(disassembly, "\n"), "")
		if err != nil {
			t.Fatalf("asm[%d]: reassemble Python disassembly: %v", index, err)
		}
		if got := canonicalString(t, fromPython); got != wantBytecode {
			t.Errorf("asm[%d] Python disassembly reassembly:\n got %s\nwant %s", index, got, wantBytecode)
		}

		ours, err := sanvm.Disassemble(bytecode)
		if err != nil {
			t.Fatalf("asm[%d]: disassemble: %v", index, err)
		}
		reassembled, err := sanvm.AssembleLines(strings.Split(ours, "\n"), "")
		if err != nil {
			t.Fatalf("asm[%d]: reassemble our disassembly: %v", index, err)
		}
		if got := canonicalString(t, reassembled); got != wantBytecode {
			t.Errorf("asm[%d] our disassembly reassembly:\n got %s\nwant %s", index, got, wantBytecode)
		}
	}
}
