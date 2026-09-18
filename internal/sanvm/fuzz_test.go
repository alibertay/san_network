package sanvm

import (
	"math/rand"
	"testing"

	"github.com/alibertay/san_network/internal/canonical"
)

// guardFuzz fails the fuzz iteration (rather than crashing the worker) when fn
// panics. This mirrors the hardening suite's guardNoPanic.
func guardFuzz(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("%s panicked: %v", name, recovered)
		}
	}()
	fn()
}

// FuzzParser covers the high-level PENA parser and compiler.
func FuzzParser(f *testing.F) {
	seeds := []string{
		"",
		"x = 1",
		"print(1 + 2 * 3)",
		"while (1) { break }",
		"function f(a, b) {\n return a + b\n}",
		"mylist := [1, 2, 3]\nmylist[0] = 4",
		"d := {}\nd[\"k\"] = 1",
		"for i, 0 -> 10 {\n print(i)\n}",
		"asm { PUSH 1 SET x }",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source string) {
		guardFuzz(t, "Parse", func() { _, _ = NewPenaParser().Parse(source) })
		guardFuzz(t, "CompilePena", func() { _, _ = CompilePena(source, "pena") })
		guardFuzz(t, "LooksLikeAssembly", func() { _ = LooksLikeAssembly(source) })
	})
}

// FuzzAssembler covers the PASM assembler and the asm compiler path.
func FuzzAssembler(f *testing.F) {
	seeds := []string{
		"PUSH 1\nHALT",
		"PUSH 1\nSET x\nGET x\nPRINT\nHALT",
		".loop:\nPUSH 1\nJMP .loop",
		"FUNC add(a, b) {\n GET a\n GET b\n ADD\n RET\n}",
		"PUSH [1, 2]\nPUSH {\"a\": 1}\nHALT",
		"LIST_APPEND items\nDICT_GET m",
		"PUSH \"unterminated",
		"JMP .missing",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source string) {
		guardFuzz(t, "Assemble", func() { _, _ = Assemble(source) })
		guardFuzz(t, "CompilePena(asm)", func() { _, _ = CompilePena(source, "asm") })
	})
}

// FuzzVM runs arbitrary bytecode assembled from the fuzz bytes. Every
// iteration is bounded by steps and gas so the fuzzer explores error paths
// without burning CPU.
func FuzzVM(f *testing.F) {
	f.Add([]byte{byte(OpPush), 1, byte(OpPrint), byte(OpHalt)})
	f.Add([]byte{byte(OpPush), 2, byte(OpPush), 3, byte(OpAdd), byte(OpHalt)})
	f.Add([]byte{byte(OpPush), 0, byte(OpJmp)})
	f.Add([]byte{byte(OpCallFunc)})
	fuzzSeeds := [][]byte{
		{byte(OpPush), 10, byte(OpDup), byte(OpMul), byte(OpPop)},
		{byte(OpPush), 1, byte(OpPush), 0, byte(OpDiv)},
	}
	for _, seed := range fuzzSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		bytecode := bytecodeFromBytes(data)
		gasLimit := int64(100_000)
		guardFuzz(t, "VM.Run", func() {
			virtualMachine := NewVM(NewStorage())
			virtualMachine.Verbose = false
			virtualMachine.SetMaxSteps(10_000)
			virtualMachine.SetGasLimit(&gasLimit)
			_ = virtualMachine.Run(bytecode, 0, &gasLimit)
		})
	})
}

// FuzzVMBytecode feeds canonical JSON bytecode (the wire format used by
// contracts and fixtures) into the VM, including malformed values.
func FuzzVMBytecode(f *testing.F) {
	seeds := []string{
		`[1, 1, 29, 0]`,
		`[1, "hello", 7, 0]`,
		`[1, [1, 2, 3], 7, 0]`,
		`[1, {"a": 1}, 7, 0]`,
		`[15]`,
		`[1, 999999999999999999999999999999, 0]`,
		`not json`,
		`{"not": "bytecode"}`,
		`[1, 1, 2, 3]`,
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source string) {
		decoded, err := canonical.Decode([]byte(source))
		if err != nil {
			return
		}
		bytecode, ok := decoded.([]any)
		if !ok {
			return
		}
		gasLimit := int64(100_000)
		guardFuzz(t, "VM.Run(bytecode)", func() {
			virtualMachine := NewVM(NewStorage())
			virtualMachine.Verbose = false
			virtualMachine.SetMaxSteps(10_000)
			_ = virtualMachine.Run(bytecode, 0, &gasLimit)
		})
	})
}

// FuzzContract deploys arbitrary decoded bytecode as a contract and calls it
// with hostile parameters, covering the contract manager and storage paths.
func FuzzContract(f *testing.F) {
	f.Add(`[1, 0, 29]`, "get", `[]`)
	f.Add(`[0]`, "", `[1, "x", [], {"a": 1}]`)
	f.Add(`null`, "set", `[1, 2, 3]`)
	f.Fuzz(func(t *testing.T, bytecodeJSON, functionName, paramsJSON string) {
		decodedBytecode, err := canonical.Decode([]byte(bytecodeJSON))
		if err != nil {
			return
		}
		bytecode, ok := decodedBytecode.([]any)
		if !ok {
			return
		}
		decodedParams, err := canonical.Decode([]byte(paramsJSON))
		if err != nil {
			return
		}
		params, ok := decodedParams.([]any)
		if !ok {
			return
		}
		gasLimit := int64(100_000)
		guardFuzz(t, "ContractManager", func() {
			virtualMachine := NewVM(NewStorage())
			virtualMachine.Verbose = false
			virtualMachine.SetMaxSteps(10_000)
			_ = virtualMachine.DeployContract("fuzz", bytecode, &gasLimit)
			_, _ = virtualMachine.CallContractFunction("fuzz", functionName, params, &gasLimit)
		})
	})
}

// bytecodeFromBytes maps fuzz bytes into instruction/value pairs. Operands are
// drawn from a small, deterministic pool so both opcode dispatch and operand
// handling are exercised.
func bytecodeFromBytes(data []byte) []any {
	values := []any{
		int64(0), int64(1), int64(-1), int64(2), int64(64),
		"", "k", "hello", true, false, nil,
		[]any{}, []any{int64(1), "two"}, map[string]any{},
		[]any{}, map[string]any{"k": int64(1)},
	}
	opcodes := []int{
		int(OpPush), int(OpPop), int(OpDrop), int(OpAdd), int(OpSub), int(OpMul),
		int(OpDiv), int(OpMod), int(OpPrint), int(OpJmp), int(OpJz), int(OpJnz),
		int(OpDup), int(OpSwap), int(OpOver), int(OpRot), int(OpAnd), int(OpOr),
		int(OpXor), int(OpEq), int(OpNeq), int(OpLt), int(OpLte), int(OpGt),
		int(OpGte), int(OpCall), int(OpRet), int(OpNop), int(OpHalt), int(OpSet),
		int(OpGet), int(OpDelete), int(OpHas), int(OpListAppend), int(OpListRemove),
		int(OpListLen), int(OpListGet), int(OpDictSet), int(OpDictGet),
		int(OpDictKeys), int(OpDefFunc), int(OpCallFunc), int(OpEndFunc),
	}
	generator := rand.New(rand.NewSource(int64(len(data))*131 + int64(byteAt(data, 0))))
	bytecode := make([]any, 0, len(data))
	for index := 0; index < len(data); index++ {
		switch data[index] % 4 {
		case 0:
			bytecode = append(bytecode, opcodes[int(data[index])%len(opcodes)])
		case 1:
			bytecode = append(bytecode, values[generator.Intn(len(values))])
		case 2:
			bytecode = append(bytecode, opcodes[int(data[index])%len(opcodes)])
			bytecode = append(bytecode, values[generator.Intn(len(values))])
		default:
			bytecode = append(bytecode, opcodes[int(data[index])%len(opcodes)])
			bytecode = append(bytecode, int(generator.Intn(16)))
		}
	}
	return bytecode
}

func byteAt(data []byte, index int) byte {
	if index < len(data) {
		return data[index]
	}
	return 0
}
