package parity

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/sanvm"
)

// parityCase is one generated SANVM program plus its execution budgets. The
// same JSON is executed by the Go VM and by the Python reference VM.
type parityCase struct {
	ID       int    `json:"id"`
	Seed     int64  `json:"seed"`
	Bytecode []any  `json:"bytecode"`
	MaxSteps int    `json:"max_steps"`
	MaxStack int    `json:"max_stack"`
	GasLimit *int64 `json:"gas_limit"`
	DictKeys bool   `json:"dict_keys"`
}

// TestDifferentialFuzzGoPythonVM is the section 15 differential fuzzer. It
// generates deterministic SANVM programs (arithmetic, stack ops, control flow,
// storage, lists/dicts, function calls and gas/step limits), executes each on
// the Go VM and the Python reference VM, and compares stack, logs, storage,
// gas used, error type and error timing (steps/pc).
//
// Default runs a small prefix; set SAN_PARITY_CASES to run thousands of cases
// (long mode) and SAN_PARITY_SEED to change the program stream. Skipped with a
// clear reason when Python is unavailable. Known intentional deviation:
// DICT_KEYS returns sorted keys in Go versus insertion order in Python; cases
// containing DICT_KEYS are compared with string-list order relaxed and the
// deviation is documented in docs/chaos-limited-consensus.md.
func TestDifferentialFuzzGoPythonVM(t *testing.T) {
	if testing.Short() {
		t.Skip("differential parity fuzzer skipped in -short mode")
	}
	python, ok := findPython()
	if !ok {
		t.Skip("differential parity fuzzer skipped: no Python 3 interpreter on PATH")
	}
	root, err := parityRepoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}

	cases := testCaseCount()
	seed := int64(1)
	if raw, ok := os.LookupEnv("SAN_PARITY_SEED"); ok {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatalf("invalid SAN_PARITY_SEED=%q: %v", raw, err)
		}
		seed = parsed
	}

	generated := make([]parityCase, 0, cases)
	for index := 0; index < cases; index++ {
		generator := rand.New(rand.NewSource(seed*1_000_003 + int64(index)))
		generated = append(generated, generateParityCase(index, generator))
	}

	goResults := make([]map[string]any, len(generated))
	for index, testCase := range generated {
		goResults[index] = goParityResult(t, testCase)
	}
	pythonResults, err := runPythonParity(python, root, generated)
	if err != nil {
		t.Fatalf("python parity driver: %v", err)
	}
	if len(pythonResults) != len(generated) {
		t.Fatalf("python returned %d result(s), want %d", len(pythonResults), len(generated))
	}

	mismatches := 0
	dictKeysCases := 0
	relaxedMatches := 0
	for index, testCase := range generated {
		if testCase.DictKeys {
			dictKeysCases++
		}
		rawGo, err := canonical.Marshal(goResults[index])
		if err != nil {
			t.Fatalf("case %d: marshal Go result: %v", index, err)
		}
		match, relaxed, detail := parityMatches(testCase, rawGo, pythonResults[index])
		if relaxed {
			relaxedMatches++
		}
		if !match {
			mismatches++
			path := persistParityMismatch(t, testCase, rawGo, pythonResults[index])
			t.Errorf("case %d (seed %d) mismatch (fixture %s):\n%s", index, testCase.Seed, path, detail)
		}
	}
	t.Logf("parity: %d case(s), %d mismatch(es), %d DICT_KEYS case(s) (%d relaxed), seed %d",
		len(generated), mismatches, dictKeysCases, relaxedMatches, seed)
	if dictKeysCases > 0 && relaxedMatches == 0 {
		t.Errorf("no DICT_KEYS case exercised the documented order whitelist; generator not observing key order")
	}
}

func testCaseCount() int {
	if raw, ok := os.LookupEnv("SAN_PARITY_CASES"); ok {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			return parsed
		}
	}
	return 128
}

// ---------------------------------------------------------------------- #
// Go execution
// ---------------------------------------------------------------------- #

func goParityResult(t *testing.T, testCase parityCase) map[string]any {
	t.Helper()
	virtualMachine := sanvm.NewVM(sanvm.NewStorage())
	virtualMachine.Verbose = false
	virtualMachine.SetMaxSteps(testCase.MaxSteps)
	virtualMachine.SetMaxStack(testCase.MaxStack)
	var gasLimit *int64
	if testCase.GasLimit != nil {
		limit := *testCase.GasLimit
		gasLimit = &limit
	}
	err := virtualMachine.Run(testCase.Bytecode, 0, gasLimit)
	return map[string]any{
		"error":    goErrorName(err),
		"gas_used": virtualMachine.GasUsed,
		"steps":    virtualMachine.Steps,
		"pc":       virtualMachine.PC,
		"stack":    virtualMachine.Stack,
		"logs":     virtualMachine.Logs,
		"storage":  virtualMachine.Storage.ToDict(),
	}
}

func goErrorName(err error) string {
	if err == nil {
		return ""
	}
	var stepLimit *sanvm.StepLimitExceeded
	if errors.As(err, &stepLimit) {
		return "StepLimitExceeded"
	}
	var stackLimit *sanvm.StackLimitExceeded
	if errors.As(err, &stackLimit) {
		return "StackLimitExceeded"
	}
	var outOfGas *sanvm.OutOfGas
	if errors.As(err, &outOfGas) {
		return "OutOfGas"
	}
	var typeError *sanvm.TypeError
	if errors.As(err, &typeError) {
		return "TypeError"
	}
	var vmError *sanvm.VMError
	if errors.As(err, &vmError) {
		return "VMError"
	}
	return fmt.Sprintf("%T", err)
}

// ---------------------------------------------------------------------- #
// Python execution
// ---------------------------------------------------------------------- #

const pythonParityDriver = `import json
import sys

root, cases_path, out_path = sys.argv[1], sys.argv[2], sys.argv[3]
sys.path.insert(0, root)

from SANVM.VM import SANVirtualMachine  # noqa: E402
from SANVM.Storage import Storage  # noqa: E402
from utils import canonical  # noqa: E402

with open(cases_path, "r", encoding="utf-8") as source, open(out_path, "w", encoding="utf-8") as sink:
    for line in source:
        line = line.strip()
        if not line:
            continue
        case = json.loads(line)
        vm = SANVirtualMachine(
            Storage(),
            max_steps=case["max_steps"],
            max_stack=case["max_stack"],
            gas_limit=case.get("gas_limit"),
            verbose=False,
        )
        failure = None
        try:
            vm.run(case["bytecode"])
        except BaseException as exc:  # noqa: BLE001
            failure = type(exc).__name__
        result = {
            "error": failure or "",
            "gas_used": vm.gas_used,
            "steps": vm.steps,
            "pc": vm.pc,
            "stack": vm.stack,
            "logs": vm.logs,
            "storage": vm.storage.to_dict(),
        }
        try:
            line = canonical.dumps(result)
        except Exception as exc:  # noqa: BLE001
            line = canonical.dumps({"error": "PythonDumpFailure", "detail": str(exc)})
        sink.write(line + "\n")
`

func findPython() (string, bool) {
	for _, candidate := range []string{"python3", "python", "py"} {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		probe := exec.Command(path, "-c", "import sys; print(sys.version_info[0])")
		output, err := probe.Output()
		if err != nil || strings.TrimSpace(string(output)) != "3" {
			continue
		}
		return path, true
	}
	return "", false
}

func runPythonParity(python, root string, cases []parityCase) ([]map[string]any, error) {
	tempDir, err := os.MkdirTemp("", "san-parity-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)

	driverPath := filepath.Join(tempDir, "parity_driver.py")
	if err := os.WriteFile(driverPath, []byte(pythonParityDriver), 0o600); err != nil {
		return nil, err
	}
	casesPath := filepath.Join(tempDir, "cases.jsonl")
	caseFile, err := os.Create(casesPath)
	if err != nil {
		return nil, err
	}
	writer := bufio.NewWriter(caseFile)
	for _, testCase := range cases {
		encoded, err := canonical.Marshal(map[string]any{
			"id":        testCase.ID,
			"bytecode":  testCase.Bytecode,
			"max_steps": testCase.MaxSteps,
			"max_stack": testCase.MaxStack,
			"gas_limit": testCase.GasLimit,
		})
		if err != nil {
			caseFile.Close()
			return nil, fmt.Errorf("marshal case %d: %w", testCase.ID, err)
		}
		writer.Write(encoded)
		writer.WriteByte('\n')
	}
	if err := writer.Flush(); err != nil {
		caseFile.Close()
		return nil, err
	}
	if err := caseFile.Close(); err != nil {
		return nil, err
	}

	resultsPath := filepath.Join(tempDir, "results.jsonl")
	command := exec.Command(python, driverPath, root, casesPath, resultsPath)
	command.Dir = root
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("%v: %s", err, stderr.String())
	}

	output, err := os.Open(resultsPath)
	if err != nil {
		return nil, err
	}
	defer output.Close()
	results := []map[string]any{}
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<26)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		decoded, err := canonical.Decode([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("decode python result: %w", err)
		}
		record, ok := decoded.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("python result is not an object")
		}
		results = append(results, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func parityRepoRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("go.mod not found above %s", directory)
		}
		directory = parent
	}
}

// ---------------------------------------------------------------------- #
// Comparison and regression fixtures
// ---------------------------------------------------------------------- #

func parityMatches(testCase parityCase, rawGo []byte, pythonResult map[string]any) (bool, bool, string) {
	rawPython, err := canonical.Marshal(pythonResult)
	if err != nil {
		return false, false, fmt.Sprintf("marshal python result: %v", err)
	}
	if bytes.Equal(rawGo, rawPython) {
		return true, false, ""
	}
	if testCase.DictKeys {
		relaxedGo, errGo := relaxStringOrder(rawGo)
		relaxedPython, errPython := relaxStringOrder(rawPython)
		if errGo == nil && errPython == nil && bytes.Equal(relaxedGo, relaxedPython) {
			return true, true, ""
		}
	}
	return false, false, fmt.Sprintf("go:     %s\npython: %s", rawGo, rawPython)
}

// relaxStringOrder re-marshals both results with any all-string list sorted, so
// the documented DICT_KEYS order deviation compares equal.
func relaxStringOrder(raw []byte) ([]byte, error) {
	decoded, err := canonical.Decode(raw)
	if err != nil {
		return nil, err
	}
	return canonical.Marshal(sortStringLists(decoded))
}

func sortStringLists(value any) any {
	switch typed := value.(type) {
	case []any:
		allStrings := len(typed) > 0
		items := make([]string, len(typed))
		for index, item := range typed {
			text, ok := item.(string)
			if !ok {
				allStrings = false
				break
			}
			items[index] = text
		}
		if allStrings {
			sort.Strings(items)
			sorted := make([]any, len(items))
			for index, item := range items {
				sorted[index] = item
			}
			return sorted
		}
		sorted := make([]any, len(typed))
		for index, item := range typed {
			sorted[index] = sortStringLists(item)
		}
		return sorted
	case map[string]any:
		sorted := make(map[string]any, len(typed))
		for key, item := range typed {
			sorted[key] = sortStringLists(item)
		}
		return sorted
	default:
		return value
	}
}

// persistParityMismatch writes a replayable regression fixture and returns its
// relative path.
func persistParityMismatch(t *testing.T, testCase parityCase, rawGo []byte, pythonResult map[string]any) string {
	t.Helper()
	directory := filepath.Join("testdata")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "<unwritable>"
	}
	digest := uint32(2166136261)
	for _, value := range testCase.Bytecode {
		encoded, err := canonical.Marshal(value)
		if err != nil {
			continue
		}
		for _, item := range encoded {
			digest = (digest ^ uint32(item)) * 16777619
		}
	}
	name := fmt.Sprintf("parity_regression_%08x.json", digest)
	path := filepath.Join(directory, name)
	payload, err := canonical.Marshal(map[string]any{
		"case":   testCase,
		"go":     string(rawGo),
		"python": pythonResult,
	})
	if err != nil {
		return path
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Logf("could not persist mismatch fixture: %v", err)
	}
	return path
}

// TestDictKeysOrderDeviationIsWhitelisted pins the one known intentional
// Go/Python VM deviation: Go DICT_KEYS returns sorted keys while Python keeps
// insertion order. The raw outcomes differ; the comparator's documented
// relaxation makes them compare equal.
func TestDictKeysOrderDeviationIsWhitelisted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short mode")
	}
	python, ok := findPython()
	if !ok {
		t.Skip("skipped: no Python 3 interpreter on PATH")
	}
	root, err := parityRepoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	testCase := parityCase{
		ID:       0,
		MaxSteps: 100_000,
		MaxStack: 1024,
		Bytecode: []any{
			int(sanvm.OpPush), "d", int(sanvm.OpPush), map[string]any{}, int(sanvm.OpSet),
			int(sanvm.OpPush), "d", int(sanvm.OpPush), "b", int(sanvm.OpPush), int64(1), int(sanvm.OpDictSet),
			int(sanvm.OpPush), "d", int(sanvm.OpPush), "a", int(sanvm.OpPush), int64(2), int(sanvm.OpDictSet),
			int(sanvm.OpPush), "d", int(sanvm.OpDictKeys), int(sanvm.OpPrint), int(sanvm.OpDrop),
			int(sanvm.OpHalt),
		},
		DictKeys: true,
	}
	rawGo, err := canonical.Marshal(goParityResult(t, testCase))
	if err != nil {
		t.Fatalf("marshal Go result: %v", err)
	}
	results, err := runPythonParity(python, root, []parityCase{testCase})
	if err != nil {
		t.Fatalf("python replay: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("python returned %d results", len(results))
	}
	rawPython, err := canonical.Marshal(results[0])
	if err != nil {
		t.Fatalf("marshal python result: %v", err)
	}
	if bytes.Equal(rawGo, rawPython) {
		t.Skip("insertion order happened to match sorted order; deviation not observable")
	}
	match, relaxed, detail := parityMatches(testCase, rawGo, results[0])
	if !match || !relaxed {
		t.Fatalf("DICT_KEYS deviation is not whitelisted:\n%s", detail)
	}
}

// TestParityRegressionFixtures replays every persisted mismatch fixture. The
// suite is empty until the fuzzer finds a mismatch; each fixture then stays in
// the repository as a regression guard.
func TestParityRegressionFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "parity_regression_*.json"))
	if err != nil {
		t.Fatalf("glob fixtures: %v", err)
	}
	if len(paths) == 0 {
		t.Skip("no parity regression fixtures recorded")
	}
	python, hasPython := findPython()
	root, err := parityRepoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		decoded, err := canonical.Decode(raw)
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		record, ok := decoded.(map[string]any)
		if !ok {
			t.Fatalf("%s is not an object", path)
		}
		caseRecord, ok := record["case"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no case", path)
		}
		testCase := parityCaseFromMap(t, caseRecord)
		rawGo, err := canonical.Marshal(goParityResult(t, testCase))
		if err != nil {
			t.Fatalf("%s: marshal Go result: %v", path, err)
		}
		if !hasPython {
			t.Logf("%s: replaying Go side only (no Python)", path)
			continue
		}
		results, err := runPythonParity(python, root, []parityCase{testCase})
		if err != nil {
			t.Fatalf("%s: python replay: %v", path, err)
		}
		if len(results) != 1 {
			t.Fatalf("%s: python returned %d results", path, len(results))
		}
		if match, _, detail := parityMatches(testCase, rawGo, results[0]); !match {
			t.Errorf("%s still mismatches:\n%s", path, detail)
		}
	}
}

func parityCaseFromMap(t *testing.T, record map[string]any) parityCase {
	t.Helper()
	encoded, err := canonical.Marshal(record)
	if err != nil {
		t.Fatalf("marshal case: %v", err)
	}
	testCase := parityCase{}
	if err := decodeParityCase(encoded, &testCase); err != nil {
		t.Fatalf("decode case: %v", err)
	}
	return testCase
}

func decodeParityCase(raw []byte, target *parityCase) error {
	decoded, err := canonical.Decode(raw)
	if err != nil {
		return err
	}
	record, ok := decoded.(map[string]any)
	if !ok {
		return fmt.Errorf("case is not an object")
	}
	if value, ok := record["id"].(int64); ok {
		target.ID = int(value)
	}
	if value, ok := record["max_steps"].(int64); ok {
		target.MaxSteps = int(value)
	}
	if value, ok := record["max_stack"].(int64); ok {
		target.MaxStack = int(value)
	}
	if value, ok := record["dict_keys"]; ok {
		if flag, ok := value.(bool); ok {
			target.DictKeys = flag
		}
	}
	if value, ok := record["gas_limit"].(int64); ok {
		target.GasLimit = &value
	}
	if bytecode, ok := record["bytecode"].([]any); ok {
		target.Bytecode = bytecode
	} else {
		return fmt.Errorf("case has no bytecode")
	}
	return nil
}

// ---------------------------------------------------------------------- #
// Deterministic program generator
// ---------------------------------------------------------------------- #

type programBuilder struct {
	items  []any
	labels map[string]int
	fixups map[string][]int
}

func newProgramBuilder() *programBuilder {
	return &programBuilder{labels: map[string]int{}, fixups: map[string][]int{}}
}

func (b *programBuilder) emit(value any) { b.items = append(b.items, value) }

func (b *programBuilder) push(value any) {
	b.emit(int(sanvm.OpPush))
	b.emit(value)
}

func (b *programBuilder) mark(label string) {
	if _, exists := b.labels[label]; !exists {
		b.labels[label] = len(b.items)
	}
}

func (b *programBuilder) jump(op int, label string) {
	b.emit(op)
	b.fixups[label] = append(b.fixups[label], len(b.items))
	b.emit(int64(0))
}

func (b *programBuilder) resolve() {
	for label, positions := range b.fixups {
		target := b.labels[label]
		for _, position := range positions {
			b.items[position] = int64(target)
		}
	}
}

func generateParityCase(id int, generator *rand.Rand) parityCase {
	seed := generator.Int63()
	builder := newProgramBuilder()
	names := []string{"v0", "v1", "v2"}
	listNames := []string{"l0", "l1"}
	dictNames := []string{"d0", "d1"}
	generatorState := &generatorState{generator: generator, builder: builder, names: names, listNames: listNames, dictNames: dictNames}

	// Seed the storage space so GET/DICT_GET/LIST_GET have targets. Raw
	// bytecode has no inline variable names: push the name, then the value,
	// then SET.
	generatorState.builder.push("v0")
	generatorState.builder.push(int64(generator.Intn(11) - 5))
	generatorState.builder.emit(int(sanvm.OpSet))
	generatorState.builder.push("v1")
	generatorState.builder.push(int64(generator.Intn(101)))
	generatorState.builder.emit(int(sanvm.OpSet))
	generatorState.builder.push("v2")
	generatorState.builder.push("seed")
	generatorState.builder.emit(int(sanvm.OpSet))
	generatorState.builder.push("l0")
	generatorState.builder.push([]any{int64(1), int64(2), "three"})
	generatorState.builder.emit(int(sanvm.OpSet))
	generatorState.builder.push("l1")
	generatorState.builder.push([]any{})
	generatorState.builder.emit(int(sanvm.OpSet))
	generatorState.builder.push("d0")
	generatorState.builder.push(map[string]any{"a": int64(1)})
	generatorState.builder.emit(int(sanvm.OpSet))
	generatorState.builder.push("d1")
	generatorState.builder.push(map[string]any{})
	generatorState.builder.emit(int(sanvm.OpSet))

	statements := 3 + generator.Intn(8)
	for index := 0; index < statements; index++ {
		generatorState.emitStatement(index)
	}

	builder.emit(int(sanvm.OpHalt))
	builder.resolve()

	maxSteps := 100_000
	maxStack := 1024
	var gasLimit *int64
	switch generator.Intn(10) {
	case 0, 1, 2:
		steps := 10 + generator.Intn(4000)
		maxSteps = steps
	case 3:
		limit := int64(1 + generator.Intn(500))
		gasLimit = &limit
	}
	if generator.Intn(20) == 0 {
		maxStack = 2 + generator.Intn(6)
	}
	return parityCase{
		ID:       id,
		Seed:     seed,
		Bytecode: builder.items,
		MaxSteps: maxSteps,
		MaxStack: maxStack,
		GasLimit: gasLimit,
		DictKeys: containsOpcode(builder.items, sanvm.OpDictKeys),
	}
}

type generatorState struct {
	generator *rand.Rand
	builder   *programBuilder
	names     []string
	listNames []string
	dictNames []string
}

func (state *generatorState) emitStatement(index int) {
	switch state.generator.Intn(12) {
	case 0:
		state.emitArithmetic()
	case 1:
		state.emitPrint()
	case 2:
		state.emitSet()
	case 3:
		state.emitCollection()
	case 4:
		state.emitStackOps()
	case 5:
		state.emitConditional(index)
	case 6:
		state.emitLoop(index)
	case 7:
		state.emitFunctionCall(index)
	case 8:
		state.emitCompare()
	case 9:
		state.emitDeleteHas()
	case 10:
		state.emitBigInteger()
	default:
		state.emitPrint()
	}
}

func (state *generatorState) emitArithmetic() {
	state.emitNumber()
	state.emitNumber()
	state.builder.emit(int([]int{int(sanvm.OpAdd), int(sanvm.OpSub), int(sanvm.OpMul), int(sanvm.OpDiv), int(sanvm.OpMod)}[state.generator.Intn(5)]))
	state.builder.emit(int(sanvm.OpDrop))
}

func (state *generatorState) emitPrint() {
	state.emitValue()
	state.builder.emit(int(sanvm.OpPrint))
	state.builder.emit(int(sanvm.OpDrop))
}

func (state *generatorState) emitSet() {
	name := state.names[state.generator.Intn(len(state.names))]
	state.builder.push(name)
	state.emitValue()
	state.builder.emit(int(sanvm.OpSet))
}

func (state *generatorState) emitCollection() {
	switch state.generator.Intn(6) {
	case 0:
		name := state.listNames[state.generator.Intn(len(state.listNames))]
		state.builder.push(name)
		state.emitValue()
		state.builder.emit(int(sanvm.OpListAppend))
	case 1:
		name := state.listNames[state.generator.Intn(len(state.listNames))]
		state.builder.push(name)
		state.emitValue()
		state.builder.emit(int(sanvm.OpListRemove))
	case 2:
		name := state.listNames[state.generator.Intn(len(state.listNames))]
		state.builder.push(name)
		state.builder.emit(int(sanvm.OpListLen))
		state.builder.emit(int(sanvm.OpDrop))
	case 3:
		name := state.listNames[state.generator.Intn(len(state.listNames))]
		state.builder.push(name)
		state.builder.push(int64(state.generator.Intn(4)))
		state.builder.emit(int(sanvm.OpListGet))
		state.builder.emit(int(sanvm.OpDrop))
	case 4:
		name := state.dictNames[state.generator.Intn(len(state.dictNames))]
		state.builder.push(name)
		state.builder.push([]string{"a", "b", "c"}[state.generator.Intn(3)])
		state.emitValue()
		state.builder.emit(int(sanvm.OpDictSet))
	default:
		name := state.dictNames[state.generator.Intn(len(state.dictNames))]
		if state.generator.Intn(2) == 0 {
			state.builder.push(name)
			state.builder.push([]string{"a", "b"}[state.generator.Intn(2)])
			state.builder.emit(int(sanvm.OpDictGet))
			state.builder.emit(int(sanvm.OpDrop))
		} else {
			// Build a dict with descending insertion order so Python's
			// insertion-order DICT_KEYS is observably different from Go's
			// sorted keys, then print the key list.
			state.builder.push(name)
			state.builder.push("c")
			state.builder.push(int64(3))
			state.builder.emit(int(sanvm.OpDictSet))
			state.builder.push(name)
			state.builder.push("b")
			state.builder.push(int64(2))
			state.builder.emit(int(sanvm.OpDictSet))
			state.builder.push(name)
			state.builder.emit(int(sanvm.OpDictKeys))
			state.builder.emit(int(sanvm.OpPrint))
			state.builder.emit(int(sanvm.OpDrop))
		}
	}
}

func (state *generatorState) emitStackOps() {
	switch state.generator.Intn(6) {
	case 0:
		state.emitNumber()
		state.builder.emit(int(sanvm.OpDup))
		state.builder.emit(int(sanvm.OpDrop))
		state.builder.emit(int(sanvm.OpDrop))
	case 1:
		state.emitNumber()
		state.emitNumber()
		state.builder.emit(int(sanvm.OpSwap))
		state.builder.emit(int(sanvm.OpDrop))
		state.builder.emit(int(sanvm.OpDrop))
	case 2:
		state.emitNumber()
		state.emitNumber()
		state.builder.emit(int(sanvm.OpOver))
		state.builder.emit(int(sanvm.OpDrop))
		state.builder.emit(int(sanvm.OpDrop))
		state.builder.emit(int(sanvm.OpDrop))
	case 3:
		state.emitNumber()
		state.emitNumber()
		state.emitNumber()
		state.builder.emit(int(sanvm.OpRot))
		state.builder.emit(int(sanvm.OpDrop))
		state.builder.emit(int(sanvm.OpDrop))
		state.builder.emit(int(sanvm.OpDrop))
	case 4:
		state.emitNumber()
		state.emitNumber()
		state.builder.emit(int(sanvm.OpPop))
		state.builder.emit(int(sanvm.OpDrop))
	default:
		state.builder.emit(int(sanvm.OpNop))
	}
}

func (state *generatorState) emitConditional(index int) {
	end := fmt.Sprintf(".end%d", index)
	state.emitNumber()
	state.builder.emit(int(sanvm.OpPush))
	state.builder.emit(int64(state.generator.Intn(5)))
	state.builder.emit(int(sanvm.OpLt))
	state.builder.jump(int(sanvm.OpJz), end)
	state.emitPrint()
	state.builder.mark(end)
}

func (state *generatorState) emitLoop(index int) {
	loop := fmt.Sprintf(".loop%d", index)
	end := fmt.Sprintf(".loopend%d", index)
	state.builder.push("v0")
	state.builder.push(int64(0))
	state.builder.emit(int(sanvm.OpSet))
	state.builder.mark(loop)
	state.builder.push("v0")
	state.builder.emit(int(sanvm.OpPush))
	state.builder.emit(int64(2 + state.generator.Intn(4)))
	state.builder.emit(int(sanvm.OpLt))
	state.builder.jump(int(sanvm.OpJz), end)
	state.emitArithmetic()
	state.builder.push("v0")
	state.builder.push("v0")
	state.builder.emit(int(sanvm.OpGet))
	state.builder.push(int64(1))
	state.builder.emit(int(sanvm.OpAdd))
	state.builder.emit(int(sanvm.OpSet))
	state.builder.jump(int(sanvm.OpJmp), loop)
	state.builder.mark(end)
}

func (state *generatorState) emitFunctionCall(index int) {
	body := fmt.Sprintf(".fn%d", index)
	after := fmt.Sprintf(".fnafter%d", index)
	// Jump over the function body; the body returns through RET.
	state.builder.jump(int(sanvm.OpJmp), after)
	state.builder.mark(body)
	state.emitNumber()
	state.builder.emit(int(sanvm.OpPrint))
	state.builder.emit(int(sanvm.OpDrop))
	state.builder.emit(int(sanvm.OpRet))
	state.builder.mark(after)
	// CALL expects the target address on the stack.
	state.builder.mark(body)
	state.builder.push(int64(0))
	state.builder.fixups[body] = append(state.builder.fixups[body], len(state.builder.items)-1)
	state.builder.emit(int(sanvm.OpCall))
}

func (state *generatorState) emitCompare() {
	if state.generator.Intn(2) == 0 {
		state.emitNumber()
		state.emitNumber()
	} else {
		state.builder.push([]string{"a", "b", "hello"}[state.generator.Intn(3)])
		state.builder.push([]string{"a", "b", "hello"}[state.generator.Intn(3)])
	}
	switch state.generator.Intn(6) {
	case 0:
		state.builder.emit(int(sanvm.OpEq))
	case 1:
		state.builder.emit(int(sanvm.OpNeq))
	case 2:
		state.builder.emit(int(sanvm.OpLt))
	case 3:
		state.builder.emit(int(sanvm.OpLte))
	case 4:
		state.builder.emit(int(sanvm.OpGt))
	default:
		state.builder.emit(int(sanvm.OpGte))
	}
	state.builder.emit(int(sanvm.OpDrop))
}

func (state *generatorState) emitDeleteHas() {
	name := state.names[state.generator.Intn(len(state.names))]
	switch state.generator.Intn(3) {
	case 0:
		state.builder.push(name)
		state.builder.emit(int(sanvm.OpDelete))
	case 1:
		state.builder.push(name)
		state.builder.emit(int(sanvm.OpHas))
		state.builder.emit(int(sanvm.OpDrop))
	default:
		state.builder.push(name)
		state.builder.emit(int(sanvm.OpGet))
		state.builder.emit(int(sanvm.OpDrop))
	}
}

func (state *generatorState) emitBigInteger() {
	if state.generator.Intn(4) == 0 {
		huge := new(big.Int).Lsh(big.NewInt(1), uint(4100+state.generator.Intn(200)))
		state.builder.push(huge) // rejected on both VMs (over MaxIntBits)
		state.builder.emit(int(sanvm.OpDrop))
		return
	}
	value := new(big.Int).Lsh(big.NewInt(1), uint(64+state.generator.Intn(400)))
	state.builder.push(value)
	state.builder.emit(int(sanvm.OpPrint))
	state.builder.emit(int(sanvm.OpDrop))
}

func (state *generatorState) emitNumber() {
	switch state.generator.Intn(6) {
	case 0:
		state.builder.push(int64(0))
	case 1:
		state.builder.push(int64(-1 - state.generator.Intn(3)))
	case 2:
		state.builder.push(int64(state.generator.Intn(50)))
	default:
		state.builder.push(int64(state.generator.Intn(1000)))
	}
}

func (state *generatorState) emitValue() {
	switch state.generator.Intn(10) {
	case 0, 1, 2, 3:
		state.emitNumber()
	case 4, 5:
		state.builder.push([]string{"", "x", "hello"}[state.generator.Intn(3)])
	case 6:
		state.builder.push([]any{int64(1), int64(2)})
	case 7:
		state.builder.push(map[string]any{"k": int64(state.generator.Intn(3))})
	case 8:
		state.builder.push([]any{[]any{int64(1)}, map[string]any{"n": []any{int64(2)}}})
	default:
		state.builder.push(int64(state.generator.Intn(2)) == 1)
	}
}

func containsOpcode(bytecode []any, opcode sanvm.OpCode) bool {
	for _, item := range bytecode {
		switch typed := item.(type) {
		case int:
			if typed == int(opcode) {
				return true
			}
		case int64:
			if int(typed) == int(opcode) {
				return true
			}
		}
	}
	return false
}
