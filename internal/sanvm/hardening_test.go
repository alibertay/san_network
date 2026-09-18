package sanvm

import (
	"math/rand"
	"strings"
	"testing"
)

// guardNoPanic fails the test when fn panics.
func guardNoPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("%s panicked: %v", name, recovered)
		}
	}()
	fn()
}

func TestAssemblerNeverPanicsOnHostileSource(t *testing.T) {
	sources := []string{
		"",
		"\x00\xff",
		"PUSH",
		"PUSH ",
		"JMP",
		"JMP nowhere",
		"PUSH 'unterminated",
		`PUSH "\xZZ"`,
		`PUSH "\U00110000"`,
		"PUSH [1,2,3",
		"PUSH {1:2,3:4",
		"FUNC f( {",
		"FUNC f(a,,b) { PUSH 1 }",
		"asm { PUSH 1 } trailing",
		"asm { PUSH 1",
		"label:",
		"label: PUSH 1",
		strings.Repeat("{", 2000),
		strings.Repeat("PUSH 1\n", 5000),
		strings.Repeat("a", 1<<16),
		"0xFFFFFFFFFFFFFFFFFFFFFFFFFFFF JMP",
	}
	for _, source := range sources {
		source := source
		guardNoPanic(t, "Assemble", func() { _, _ = Assemble(source) })
		guardNoPanic(t, "CompilePena(asm)", func() { _, _ = CompilePena(source, "asm") })
		guardNoPanic(t, "CompilePena(pena)", func() { _, _ = CompilePena(source, "pena") })
		guardNoPanic(t, "LooksLikeAssembly", func() { _ = LooksLikeAssembly(source) })
	}
}

func TestEncodeRejectsMalformedInstructions(t *testing.T) {
	cases := [][]Instruction{
		{{Name: LabelRecord}},
		{{Name: LabelRecord, Operands: []any{int64(1)}}},
		{{Name: LabelRecord, Operands: []any{"ok"}}, {Name: LabelRecord, Operands: []any{"ok"}}},
		{{Name: "PUSH"}},
		{{Name: "JMP"}},
		{{Name: "JMP", Operands: []any{"missing-label"}}},
		{{Name: "JMP", Operands: []any{int64(-1)}}},
		{{Name: "NOPE"}},
	}
	for index, instructions := range cases {
		guardNoPanic(t, "Encode", func() {
			if _, err := Encode(instructions); err == nil {
				t.Errorf("case %d: Encode accepted malformed instructions", index)
			}
		})
	}
}

func TestParseInstructionListNeverPanics(t *testing.T) {
	cases := [][]any{
		nil,
		{},
		{[]any{}},
		{int64(1)},
		{[]any{int64(1)}},
		{[]any{"PUSH"}},
		{[]any{"PUSH", nil}},
		{[]any{"PUSH", map[string]any{"deep": []any{1, 2}}}},
		{[]any{"UNKNOWN"}},
		{[]any{"HALT", "extra"}},
		{[]any{nil, "HALT"}},
	}
	for index, instructions := range cases {
		guardNoPanic(t, "ParseInstructionList", func() {
			bytecode, err := ParseInstructionList(instructions)
			if err == nil {
				guardNoPanic(t, "Disassemble", func() { _, _ = Disassemble(bytecode) })
			}
		})
		_ = index
	}
}

func TestDisassembleNeverPanics(t *testing.T) {
	bytecodes := [][]any{
		nil,
		{},
		{nil},
		{"skipped", "skipped"},
		{int(OpPush)},
		{int(OpJmp)},
		{int(OpJmp), "not-a-target"},
		{int(OpHalt), int(OpHalt)},
		{int(999999)},
	}
	for index, bytecode := range bytecodes {
		guardNoPanic(t, "Disassemble", func() { _, _ = Disassemble(bytecode) })
		_ = index
	}
}

func TestVMNeverPanicsOnArbitraryBytecode(t *testing.T) {
	generator := rand.New(rand.NewSource(1))
	values := []any{
		nil, true, false, int64(0), int64(1), int64(-1), int(0), int(1),
		"text", "", 3.5, -0.0, []any{1, "two"}, map[string]any{"k": "v"},
		[]any{}, map[string]any{},
	}
	for program := 0; program < 2000; program++ {
		size := generator.Intn(40)
		bytecode := make([]any, 0, size)
		for i := 0; i < size; i++ {
			if generator.Intn(3) == 0 {
				bytecode = append(bytecode, generator.Intn(64))
			} else {
				bytecode = append(bytecode, values[generator.Intn(len(values))])
			}
		}
		limit := int64(generator.Intn(1000))
		guardNoPanic(t, "VM.Run", func() {
			vm := NewVM(NewStorage())
			vm.Verbose = false
			_ = vm.Run(bytecode, 0, &limit)
		})
	}
}

func FuzzAssemble(f *testing.F) {
	seeds := []string{
		"PUSH 1\nHALT",
		"asm { PUSH \"x\" }",
		"FUNC f(a) { PUSH a GET RET }",
		"JMP .L1\n.L1: HALT",
		"PUSH [1,2,3]\nPUSH {\"a\":1}\nHALT",
		"label: PUSH \\xZZ\nHALT",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source string) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("Assemble(%q) panicked: %v", source, recovered)
			}
		}()
		_, _ = Assemble(source)
		_, _ = CompilePena(source, "pena")
	})
}
