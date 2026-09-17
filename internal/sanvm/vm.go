package sanvm

import (
	"fmt"
	"math"
	"math/big"
	"sort"

	"github.com/alibertay/san_network/internal/canonical"
)

// VMError marks a deterministic VM failure (bad opcode, limits, operand...).
type VMError struct{ Message string }

func (e *VMError) Error() string { return e.Message }

func vmErrorf(format string, args ...any) *VMError {
	return &VMError{Message: fmt.Sprintf(format, args...)}
}

// StepLimitExceeded marks a step budget overrun.
type StepLimitExceeded struct{ VMError }

// StackLimitExceeded marks a stack budget overrun.
type StackLimitExceeded struct{ VMError }

// OutOfGas marks a gas budget overrun.
type OutOfGas struct{ VMError }

// VM defaults (mirrors SANVM/VM.py).
const (
	DefaultMaxSteps     = 100_000
	DefaultMaxStack     = 1024
	DefaultMaxCallDepth = 64
	MaxValueBytes       = 65_536
)

// VM is the stack-based SAN virtual machine.
type VM struct {
	Storage         *Storage
	ContractManager *ContractManager

	MaxSteps     int
	MaxStack     int
	MaxCallDepth int
	GasLimit     *int64
	GasUsed      int64
	Verbose      bool
	Logs         []map[string]any

	Stack     []any
	callStack []int64
	scopes    []map[string]any
	running   bool
	PC        int
	Steps     int
	bytecode  []any
}

// NewVM builds a VM around a storage.
func NewVM(storage *Storage) *VM {
	if storage == nil {
		storage = NewStorage()
	}
	virtualMachine := &VM{
		Storage:      storage,
		MaxSteps:     DefaultMaxSteps,
		MaxStack:     DefaultMaxStack,
		MaxCallDepth: DefaultMaxCallDepth,
		Verbose:      true,
		Logs:         []map[string]any{},
	}
	virtualMachine.ContractManager = NewContractManager(storage)
	return virtualMachine
}

// SetMaxSteps overrides the step budget.
func (virtualMachine *VM) SetMaxSteps(steps int) { virtualMachine.MaxSteps = steps }

// SetMaxStack overrides the stack budget.
func (virtualMachine *VM) SetMaxStack(size int) { virtualMachine.MaxStack = size }

// SetGasLimit sets an optional gas budget (nil means unlimited).
func (virtualMachine *VM) SetGasLimit(limit *int64) { virtualMachine.GasLimit = limit }

// Run executes bytecode starting at startPC; state is reset on every call.
func (virtualMachine *VM) Run(bytecode []any, startPC int, gasLimit *int64) error {
	virtualMachine.bytecode = append([]any{}, bytecode...)
	virtualMachine.Stack = []any{}
	virtualMachine.callStack = []int64{}
	virtualMachine.scopes = []map[string]any{}
	virtualMachine.running = true
	virtualMachine.Steps = 0
	virtualMachine.PC = startPC
	if gasLimit != nil {
		virtualMachine.GasLimit = gasLimit
	}
	virtualMachine.GasUsed = 0
	virtualMachine.Logs = []map[string]any{}

	for virtualMachine.running && virtualMachine.PC >= 0 && virtualMachine.PC < len(virtualMachine.bytecode) {
		if virtualMachine.Steps >= virtualMachine.MaxSteps {
			return &StepLimitExceeded{VMError{Message: fmt.Sprintf(
				"Execution exceeded %d steps", virtualMachine.MaxSteps)}}
		}
		virtualMachine.Steps++

		opcode := virtualMachine.bytecode[virtualMachine.PC]
		virtualMachine.PC++
		if value, ok := opcode.(int); ok {
			if err := virtualMachine.charge(InstructionCost(value)); err != nil {
				return err
			}
		} else if value, ok := opcode.(int64); ok {
			if err := virtualMachine.charge(InstructionCost(int(value))); err != nil {
				return err
			}
		}
		if err := virtualMachine.execute(opcode); err != nil {
			return err
		}
	}
	return nil
}

// Execute runs bytecode with default options.
func (virtualMachine *VM) Execute(bytecode []any) error {
	return virtualMachine.Run(bytecode, 0, nil)
}

func (virtualMachine *VM) execute(opcode any) error {
	value, ok := opcodeInt(opcode)
	if !ok {
		return vmErrorf("Unknown opcode: %v at pc %d", opcode, virtualMachine.PC-1)
	}
	switch value {
	case int(OpPush):
		return virtualMachine.push()
	case int(OpPop), int(OpDrop):
		virtualMachine.popTop()
		return nil
	case int(OpAdd):
		return virtualMachine.arith("+")
	case int(OpSub):
		return virtualMachine.arith("-")
	case int(OpMul):
		return virtualMachine.arith("*")
	case int(OpDiv):
		return virtualMachine.arith("/")
	case int(OpMod):
		return virtualMachine.arith("%")
	case int(OpPrint):
		return virtualMachine.printTop()
	case int(OpHalt):
		virtualMachine.running = false
		return nil
	case int(OpJmp):
		return virtualMachine.jmp()
	case int(OpJz):
		return virtualMachine.jumpIf(false)
	case int(OpJnz):
		return virtualMachine.jumpIf(true)
	case int(OpDup):
		return virtualMachine.dup()
	case int(OpSwap):
		virtualMachine.swap()
		return nil
	case int(OpOver):
		return virtualMachine.over()
	case int(OpRot):
		virtualMachine.rot()
		return nil
	case int(OpAnd):
		return virtualMachine.logic("and")
	case int(OpOr):
		return virtualMachine.logic("or")
	case int(OpXor):
		return virtualMachine.logic("xor")
	case int(OpEq):
		return virtualMachine.compareOp("eq")
	case int(OpNeq):
		return virtualMachine.compareOp("neq")
	case int(OpLt):
		return virtualMachine.compareOp("lt")
	case int(OpLte):
		return virtualMachine.compareOp("lte")
	case int(OpGt):
		return virtualMachine.compareOp("gt")
	case int(OpGte):
		return virtualMachine.compareOp("gte")
	case int(OpCall):
		return virtualMachine.call()
	case int(OpRet), int(OpEndFunc):
		virtualMachine.ret()
		return nil
	case int(OpNop):
		return nil
	case int(OpSet):
		return virtualMachine.setVar()
	case int(OpGet):
		return virtualMachine.getVar()
	case int(OpDelete):
		return virtualMachine.deleteVar()
	case int(OpHas):
		return virtualMachine.hasVar()
	case int(OpListAppend):
		return virtualMachine.listAppend()
	case int(OpListRemove):
		return virtualMachine.listRemove()
	case int(OpListLen):
		return virtualMachine.listLen()
	case int(OpListGet):
		return virtualMachine.listGet()
	case int(OpDictSet):
		return virtualMachine.dictSet()
	case int(OpDictGet):
		return virtualMachine.dictGet()
	case int(OpDictKeys):
		return virtualMachine.dictKeys()
	case int(OpDefFunc):
		return virtualMachine.defineFunction()
	case int(OpCallFunc):
		return virtualMachine.callFunction()
	default:
		return vmErrorf("Unknown opcode: %v at pc %d", opcode, virtualMachine.PC-1)
	}
}

func opcodeInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	default:
		return 0, false
	}
}

func (virtualMachine *VM) charge(cost int) error {
	virtualMachine.GasUsed += int64(cost)
	if virtualMachine.GasLimit != nil && virtualMachine.GasUsed > *virtualMachine.GasLimit {
		return &OutOfGas{VMError{Message: fmt.Sprintf(
			"Out of gas: used %d, limit %d", virtualMachine.GasUsed, *virtualMachine.GasLimit)}}
	}
	return nil
}

// ---------------------------------------------------------------------- #
// Helpers
// ---------------------------------------------------------------------- #

func (virtualMachine *VM) pushValue(value any) error {
	if len(virtualMachine.Stack) >= virtualMachine.MaxStack {
		return &StackLimitExceeded{VMError{Message: fmt.Sprintf(
			"Stack exceeded %d items", virtualMachine.MaxStack)}}
	}
	switch typed := value.(type) {
	case []any, map[string]any:
		value = deepCopy(typed)
		encoded, err := canonical.Marshal(value)
		if err != nil {
			return &VMError{Message: err.Error()}
		}
		if len(encoded) > MaxValueBytes {
			return vmErrorf("Value exceeds %d bytes", MaxValueBytes)
		}
	case string:
		if len([]byte(typed)) > MaxValueBytes {
			return vmErrorf("Value exceeds %d bytes", MaxValueBytes)
		}
	default:
		if !isFloat(value) {
			if bigValue, ok := toBig(value); ok {
				if !isBool(value) && bigValue.BitLen() > MaxIntBits {
					return vmErrorf("Integer exceeds %d bits", MaxIntBits)
				}
			}
		}
	}
	virtualMachine.Stack = append(virtualMachine.Stack, value)
	return nil
}

func isBool(value any) bool {
	_, ok := value.(bool)
	return ok
}

func (virtualMachine *VM) popValue() (any, bool) {
	if len(virtualMachine.Stack) == 0 {
		return nil, false
	}
	value := virtualMachine.Stack[len(virtualMachine.Stack)-1]
	virtualMachine.Stack = virtualMachine.Stack[:len(virtualMachine.Stack)-1]
	return value, true
}

func (virtualMachine *VM) popTop() {
	if len(virtualMachine.Stack) > 0 {
		virtualMachine.Stack = virtualMachine.Stack[:len(virtualMachine.Stack)-1]
	}
}

func (virtualMachine *VM) require(count int) error {
	for len(virtualMachine.Stack) < count {
		if err := virtualMachine.pushValue(int64(0)); err != nil {
			return err
		}
	}
	return nil
}

func (virtualMachine *VM) readTarget() (int64, error) {
	if virtualMachine.PC >= len(virtualMachine.bytecode) {
		return 0, vmErrorf("Jump/function target is missing")
	}
	target := virtualMachine.bytecode[virtualMachine.PC]
	virtualMachine.PC++
	number, ok := opcodeInt(target)
	if !ok || number < 0 || number > len(virtualMachine.bytecode) {
		return 0, vmErrorf("Invalid jump target: %v", target)
	}
	return int64(number), nil
}

func (virtualMachine *VM) wordCost(a, b any) int {
	if isInteger(a) && isInteger(b) {
		left := intBitLength(a)
		right := intBitLength(b)
		if right > left {
			left = right
		}
		return left / 64
	}
	return 0
}

// ---------------------------------------------------------------------- #
// Stack and arithmetic
// ---------------------------------------------------------------------- #

func (virtualMachine *VM) push() error {
	if virtualMachine.PC >= len(virtualMachine.bytecode) {
		return vmErrorf("PUSH without operand")
	}
	value := virtualMachine.bytecode[virtualMachine.PC]
	virtualMachine.PC++
	if err := virtualMachine.charge(PayloadCost(value)); err != nil {
		return err
	}
	return virtualMachine.pushValue(value)
}

func (virtualMachine *VM) arith(operator string) error {
	if err := virtualMachine.require(2); err != nil {
		return err
	}
	right, _ := virtualMachine.popValue()
	left, _ := virtualMachine.popValue()
	if err := virtualMachine.charge(virtualMachine.wordCost(left, right)); err != nil {
		return err
	}
	result, err := binaryOp(operator, left, right)
	if err != nil {
		return &VMError{Message: err.Error()}
	}
	return virtualMachine.pushValue(result)
}

func binaryOp(operator string, left, right any) (any, error) {
	if operator == "+" {
		if leftText, ok := left.(string); ok {
			if rightText, ok := right.(string); ok {
				return leftText + rightText, nil
			}
		}
		if leftList, ok := left.([]any); ok {
			if rightList, ok := right.([]any); ok {
				combined := make([]any, 0, len(leftList)+len(rightList))
				combined = append(combined, leftList...)
				combined = append(combined, rightList...)
				return combined, nil
			}
		}
	}

	if isInteger(left) && isInteger(right) && !isFloat(left) && !isFloat(right) {
		leftBig, _ := toBig(left)
		rightBig, _ := toBig(right)
		result := new(big.Int)
		switch operator {
		case "+":
			result.Add(leftBig, rightBig)
		case "-":
			result.Sub(leftBig, rightBig)
		case "*":
			result.Mul(leftBig, rightBig)
		case "/":
			divided, err := pythonFloorDivInt(leftBig, rightBig)
			if err != nil {
				return nil, err
			}
			return normalizeInt(divided), nil
		case "%":
			remainder, err := pythonModInt(leftBig, rightBig)
			if err != nil {
				return nil, err
			}
			return normalizeInt(remainder), nil
		default:
			return nil, fmt.Errorf("Unsupported operator: %s", operator)
		}
		return normalizeInt(result), nil
	}

	leftFloat, leftOK := toFloat(left)
	rightFloat, rightOK := toFloat(right)
	if !leftOK || !rightOK {
		return nil, fmt.Errorf("Unsupported operand types for %s: %T and %T", operator, left, right)
	}
	switch operator {
	case "+":
		return leftFloat + rightFloat, nil
	case "-":
		return leftFloat - rightFloat, nil
	case "*":
		return leftFloat * rightFloat, nil
	case "/":
		if rightFloat == 0 {
			return nil, fmt.Errorf("Division by zero")
		}
		return leftFloat / rightFloat, nil
	case "%":
		return pythonModFloat(leftFloat, rightFloat)
	default:
		return nil, fmt.Errorf("Unsupported operator: %s", operator)
	}
}

func (virtualMachine *VM) printTop() error {
	if len(virtualMachine.Stack) == 0 {
		return nil
	}
	value := virtualMachine.Stack[len(virtualMachine.Stack)-1]
	virtualMachine.Logs = append(virtualMachine.Logs, map[string]any{"event": "print", "value": value})
	if virtualMachine.Verbose {
		fmt.Println(pythonRepr(value))
	}
	return nil
}

func pythonRepr(value any) string {
	switch typed := value.(type) {
	case string:
		return fmt.Sprintf("%q", typed)
	default:
		encoded, err := canonical.Marshal(value)
		if err != nil {
			return fmt.Sprintf("%v", value)
		}
		return string(encoded)
	}
}

// ---------------------------------------------------------------------- #
// Control flow
// ---------------------------------------------------------------------- #

func (virtualMachine *VM) jmp() error {
	target, err := virtualMachine.readTarget()
	if err != nil {
		return err
	}
	virtualMachine.PC = int(target)
	return nil
}

func (virtualMachine *VM) jumpIf(when bool) error {
	if err := virtualMachine.require(1); err != nil {
		return err
	}
	condition, _ := virtualMachine.popValue()
	target, err := virtualMachine.readTarget()
	if err != nil {
		return err
	}
	if truthy(condition) == when {
		virtualMachine.PC = int(target)
	}
	return nil
}

// ---------------------------------------------------------------------- #
// Stack manipulation
// ---------------------------------------------------------------------- #

func (virtualMachine *VM) dup() error {
	if len(virtualMachine.Stack) > 0 {
		return virtualMachine.pushValue(virtualMachine.Stack[len(virtualMachine.Stack)-1])
	}
	return nil
}

func (virtualMachine *VM) swap() {
	if len(virtualMachine.Stack) >= 2 {
		top := len(virtualMachine.Stack) - 1
		virtualMachine.Stack[top], virtualMachine.Stack[top-1] =
			virtualMachine.Stack[top-1], virtualMachine.Stack[top]
	}
}

func (virtualMachine *VM) over() error {
	if len(virtualMachine.Stack) >= 2 {
		return virtualMachine.pushValue(virtualMachine.Stack[len(virtualMachine.Stack)-2])
	}
	return nil
}

func (virtualMachine *VM) rot() {
	if len(virtualMachine.Stack) >= 3 {
		top := len(virtualMachine.Stack) - 1
		a, b, c := virtualMachine.Stack[top-2], virtualMachine.Stack[top-1], virtualMachine.Stack[top]
		virtualMachine.Stack[top-2], virtualMachine.Stack[top-1], virtualMachine.Stack[top] = b, c, a
	}
}

// ---------------------------------------------------------------------- #
// Comparisons and logic
// ---------------------------------------------------------------------- #

func (virtualMachine *VM) logic(operation string) error {
	if err := virtualMachine.require(2); err != nil {
		return err
	}
	right, _ := virtualMachine.popValue()
	left, _ := virtualMachine.popValue()
	var result int64
	switch operation {
	case "and":
		if truthy(left) && truthy(right) {
			result = 1
		}
	case "or":
		if truthy(left) || truthy(right) {
			result = 1
		}
	case "xor":
		if truthy(left) != truthy(right) {
			result = 1
		}
	}
	return virtualMachine.pushValue(result)
}

func (virtualMachine *VM) compareOp(operation string) error {
	if err := virtualMachine.require(2); err != nil {
		return err
	}
	right, _ := virtualMachine.popValue()
	left, _ := virtualMachine.popValue()

	equal := valuesEqual(left, right)
	switch operation {
	case "eq":
		return virtualMachine.pushValue(boolToInt(equal || (isEmptyValue(left) && isEmptyValue(right))))
	case "neq":
		return virtualMachine.pushValue(boolToInt(!(equal || (isEmptyValue(left) && isEmptyValue(right)))))
	}

	if isNaN(left) || isNaN(right) {
		// Python comparisons with NaN are always false.
		return virtualMachine.pushValue(int64(0))
	}

	order, err := valuesOrder(left, right)
	if err != nil {
		return &VMError{Message: err.Error()}
	}
	switch operation {
	case "lt":
		return virtualMachine.pushValue(boolToInt(order < 0))
	case "lte":
		return virtualMachine.pushValue(boolToInt(order <= 0))
	case "gt":
		return virtualMachine.pushValue(boolToInt(order > 0))
	case "gte":
		return virtualMachine.pushValue(boolToInt(order >= 0))
	}
	return nil
}

func boolToInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func isNaN(value any) bool {
	if floatValue, ok := value.(float64); ok {
		return math.IsNaN(floatValue)
	}
	return false
}

func valuesOrder(left, right any) (int, error) {
	if isFloat(left) || isFloat(right) {
		leftFloat, leftOK := toFloat(left)
		rightFloat, rightOK := toFloat(right)
		if !leftOK || !rightOK {
			return 0, fmt.Errorf("Unsupported operand types for comparison")
		}
		if math.IsNaN(leftFloat) || math.IsNaN(rightFloat) {
			return 0, nil
		}
		switch {
		case leftFloat < rightFloat:
			return -1, nil
		case leftFloat > rightFloat:
			return 1, nil
		default:
			return 0, nil
		}
	}
	if isInteger(left) && isInteger(right) {
		return compareNumbers(left, right), nil
	}
	if leftText, ok := left.(string); ok {
		if rightText, ok := right.(string); ok {
			switch {
			case leftText < rightText:
				return -1, nil
			case leftText > rightText:
				return 1, nil
			default:
				return 0, nil
			}
		}
	}
	if leftList, ok := left.([]any); ok {
		if rightList, ok := right.([]any); ok {
			return compareLists(leftList, rightList)
		}
	}
	return 0, fmt.Errorf("Unsupported operand types for comparison")
}

func compareLists(left, right []any) (int, error) {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for i := 0; i < limit; i++ {
		if valuesEqual(left[i], right[i]) {
			continue
		}
		return valuesOrder(left[i], right[i])
	}
	switch {
	case len(left) < len(right):
		return -1, nil
	case len(left) > len(right):
		return 1, nil
	default:
		return 0, nil
	}
}

// ---------------------------------------------------------------------- #
// Variables (local scope first, then contract storage)
// ---------------------------------------------------------------------- #

func (virtualMachine *VM) setVar() error {
	if err := virtualMachine.require(2); err != nil {
		return err
	}
	value, _ := virtualMachine.popValue()
	key, _ := virtualMachine.popValue()
	keyText := StorageKey(key)
	for i := len(virtualMachine.scopes) - 1; i >= 0; i-- {
		if _, ok := virtualMachine.scopes[i][keyText]; ok {
			virtualMachine.scopes[i][keyText] = value
			return nil
		}
	}
	virtualMachine.Storage.SetVar(key, value)
	return nil
}

func (virtualMachine *VM) getVar() error {
	if err := virtualMachine.require(1); err != nil {
		return err
	}
	key, _ := virtualMachine.popValue()
	keyText := StorageKey(key)
	for i := len(virtualMachine.scopes) - 1; i >= 0; i-- {
		if value, ok := virtualMachine.scopes[i][keyText]; ok {
			return virtualMachine.pushValue(value)
		}
	}
	return virtualMachine.pushValue(virtualMachine.Storage.GetVar(key))
}

func (virtualMachine *VM) deleteVar() error {
	if err := virtualMachine.require(1); err != nil {
		return err
	}
	key, _ := virtualMachine.popValue()
	virtualMachine.Storage.DeleteVar(key)
	return nil
}

func (virtualMachine *VM) hasVar() error {
	if err := virtualMachine.require(1); err != nil {
		return err
	}
	key, _ := virtualMachine.popValue()
	return virtualMachine.pushValue(boolToInt(virtualMachine.Storage.HasVar(key)))
}

// ---------------------------------------------------------------------- #
// Collections
// ---------------------------------------------------------------------- #

func (virtualMachine *VM) listAppend() error {
	if err := virtualMachine.require(2); err != nil {
		return err
	}
	value, _ := virtualMachine.popValue()
	key, _ := virtualMachine.popValue()
	if err := virtualMachine.charge(PayloadCost(value)); err != nil {
		return err
	}
	if !virtualMachine.Storage.HasVar(key) {
		return vmErrorf("Unknown list: %v", key)
	}
	list, ok := virtualMachine.Storage.GetVar(key).([]any)
	if !ok {
		return vmErrorf("%v is not a list", key)
	}
	list = append(list, value)
	virtualMachine.Storage.SetVar(key, list)
	return nil
}

func (virtualMachine *VM) listRemove() error {
	if err := virtualMachine.require(2); err != nil {
		return err
	}
	value, _ := virtualMachine.popValue()
	key, _ := virtualMachine.popValue()
	if err := virtualMachine.charge(PayloadCost(value)); err != nil {
		return err
	}
	if !virtualMachine.Storage.HasVar(key) {
		return vmErrorf("Unknown list: %v", key)
	}
	list, ok := virtualMachine.Storage.GetVar(key).([]any)
	if !ok {
		return vmErrorf("%v is not a list", key)
	}
	for index, item := range list {
		if valuesEqual(item, value) {
			list = append(list[:index], list[index+1:]...)
			break
		}
	}
	virtualMachine.Storage.SetVar(key, list)
	return nil
}

func (virtualMachine *VM) listLen() error {
	if err := virtualMachine.require(1); err != nil {
		return err
	}
	key, _ := virtualMachine.popValue()
	if !virtualMachine.Storage.HasVar(key) {
		return virtualMachine.pushValue(int64(0))
	}
	list, ok := virtualMachine.Storage.GetVar(key).([]any)
	if !ok {
		return vmErrorf("%v is not a list", key)
	}
	return virtualMachine.pushValue(int64(len(list)))
}

func (virtualMachine *VM) listGet() error {
	if err := virtualMachine.require(2); err != nil {
		return err
	}
	indexValue, _ := virtualMachine.popValue()
	key, _ := virtualMachine.popValue()
	if !virtualMachine.Storage.HasVar(key) {
		return vmErrorf("Unknown list: %v", key)
	}
	list, ok := virtualMachine.Storage.GetVar(key).([]any)
	if !ok {
		return vmErrorf("%v is not a list", key)
	}
	index, ok := toBig(indexValue)
	if !ok || !index.IsInt64() {
		return vmErrorf("%v is an invalid index for %v", indexValue, key)
	}
	position := index.Int64()
	if position < 0 || position >= int64(len(list)) {
		return vmErrorf("%v is an invalid index for %v", indexValue, key)
	}
	return virtualMachine.pushValue(list[position])
}

func (virtualMachine *VM) dictSet() error {
	if err := virtualMachine.require(3); err != nil {
		return err
	}
	value, _ := virtualMachine.popValue()
	keyName, _ := virtualMachine.popValue()
	dictName, _ := virtualMachine.popValue()
	if err := virtualMachine.charge(PayloadCost(value) + PayloadCost(keyName)); err != nil {
		return err
	}
	if !virtualMachine.Storage.HasVar(dictName) {
		return vmErrorf("Unknown subscript target: %v", dictName)
	}
	container := virtualMachine.Storage.GetVar(dictName)
	switch typed := container.(type) {
	case map[string]any:
		typed[StorageKey(keyName)] = value
	case []any:
		index, ok := toBig(keyName)
		if !ok || !index.IsInt64() || index.Int64() < 0 || index.Int64() > int64(len(typed)) {
			return vmErrorf("%v is an invalid index for %v", keyName, dictName)
		}
		position := index.Int64()
		if position == int64(len(typed)) {
			typed = append(typed, value)
		} else {
			typed[position] = value
		}
		container = typed
	default:
		return vmErrorf("%v is not subscriptable", dictName)
	}
	virtualMachine.Storage.SetVar(dictName, container)
	return nil
}

func (virtualMachine *VM) dictGet() error {
	if err := virtualMachine.require(2); err != nil {
		return err
	}
	keyName, _ := virtualMachine.popValue()
	dictName, _ := virtualMachine.popValue()
	if !virtualMachine.Storage.HasVar(dictName) {
		return vmErrorf("Unknown subscript target: %v", dictName)
	}
	container := virtualMachine.Storage.GetVar(dictName)
	switch typed := container.(type) {
	case map[string]any:
		if value, ok := typed[StorageKey(keyName)]; ok {
			return virtualMachine.pushValue(value)
		}
		return virtualMachine.pushValue(int64(0))
	case []any:
		index, ok := toBig(keyName)
		if !ok || !index.IsInt64() || index.Int64() < 0 || index.Int64() >= int64(len(typed)) {
			return vmErrorf("%v is an invalid index for %v", keyName, dictName)
		}
		return virtualMachine.pushValue(typed[index.Int64()])
	default:
		return vmErrorf("%v is not subscriptable", dictName)
	}
}

func (virtualMachine *VM) dictKeys() error {
	if err := virtualMachine.require(1); err != nil {
		return err
	}
	dictName, _ := virtualMachine.popValue()
	if !virtualMachine.Storage.HasVar(dictName) {
		return vmErrorf("Unknown dict: %v", dictName)
	}
	dictionary, ok := virtualMachine.Storage.GetVar(dictName).(map[string]any)
	if !ok {
		return vmErrorf("%v is not a dict", dictName)
	}
	names := make([]string, 0, len(dictionary))
	for key := range dictionary {
		names = append(names, key)
	}
	sort.Strings(names)
	keys := make([]any, 0, len(names))
	for _, key := range names {
		keys = append(keys, key)
	}
	return virtualMachine.pushValue(keys)
}

// ---------------------------------------------------------------------- #
// Calls and functions
// ---------------------------------------------------------------------- #

func (virtualMachine *VM) call() error {
	if err := virtualMachine.require(1); err != nil {
		return err
	}
	addressValue, _ := virtualMachine.popValue()
	address, ok := toBig(addressValue)
	if !ok || !address.IsInt64() {
		return vmErrorf("Invalid call address: %v", addressValue)
	}
	if len(virtualMachine.callStack) >= virtualMachine.MaxCallDepth {
		return vmErrorf("Call depth exceeded %d", virtualMachine.MaxCallDepth)
	}
	virtualMachine.callStack = append(virtualMachine.callStack, int64(virtualMachine.PC))
	virtualMachine.PC = int(address.Int64())
	return nil
}

func (virtualMachine *VM) ret() {
	if len(virtualMachine.callStack) == 0 {
		return
	}
	frame := virtualMachine.callStack[len(virtualMachine.callStack)-1]
	virtualMachine.callStack = virtualMachine.callStack[:len(virtualMachine.callStack)-1]
	if len(virtualMachine.scopes) > 0 {
		virtualMachine.scopes = virtualMachine.scopes[:len(virtualMachine.scopes)-1]
	}
	virtualMachine.PC = int(frame)
}

func (virtualMachine *VM) defineFunction() error {
	if err := virtualMachine.require(2); err != nil {
		return err
	}
	parameterCountValue, _ := virtualMachine.popValue()
	parameterCount, ok := toBig(parameterCountValue)
	if !ok || parameterCount.Sign() < 0 || !parameterCount.IsInt64() {
		return vmErrorf("Invalid parameter count: %v", parameterCountValue)
	}
	count := int(parameterCount.Int64())
	if err := virtualMachine.require(count + 1); err != nil {
		return err
	}
	parameters := make([]any, count)
	for i := 0; i < count; i++ {
		parameters[i], _ = virtualMachine.popValue()
	}
	// Reverse (Python pops then reverses).
	for i, j := 0, len(parameters)-1; i < j; i, j = i+1, j-1 {
		parameters[i], parameters[j] = parameters[j], parameters[i]
	}
	functionName, _ := virtualMachine.popValue()
	bodyPC, err := virtualMachine.readTarget()
	if err != nil {
		return err
	}
	virtualMachine.Storage.Functions[StorageKey(functionName)] = map[string]any{
		"pc":          bodyPC,
		"params":      parameters,
		"param_count": int64(count),
	}
	return nil
}

func (virtualMachine *VM) callFunction() error {
	if err := virtualMachine.require(2); err != nil {
		return err
	}
	parameterCountValue, _ := virtualMachine.popValue()
	functionName, _ := virtualMachine.popValue()
	parameterCount, ok := toBig(parameterCountValue)
	if !ok || parameterCount.Sign() < 0 || !parameterCount.IsInt64() {
		return vmErrorf("Invalid parameter count: %v", parameterCountValue)
	}
	count := int(parameterCount.Int64())

	info, ok := virtualMachine.Storage.Functions[StorageKey(functionName)]
	if !ok {
		return vmErrorf("Unknown function: %v", functionName)
	}
	expected := int(toInt64Value(info["param_count"]))
	if expected != count {
		return vmErrorf("%v expects %d parameter(s), got %d", functionName, expected, count)
	}
	if len(virtualMachine.callStack) >= virtualMachine.MaxCallDepth {
		return vmErrorf("Call depth exceeded %d", virtualMachine.MaxCallDepth)
	}
	if err := virtualMachine.require(count); err != nil {
		return err
	}
	arguments := make([]any, count)
	for i := 0; i < count; i++ {
		arguments[i], _ = virtualMachine.popValue()
	}
	for i, j := 0, len(arguments)-1; i < j; i, j = i+1, j-1 {
		arguments[i], arguments[j] = arguments[j], arguments[i]
	}

	parameters, _ := info["params"].([]any)
	scope := map[string]any{}
	for index, parameter := range parameters {
		scope[StorageKey(parameter)] = arguments[index]
	}
	virtualMachine.callStack = append(virtualMachine.callStack, int64(virtualMachine.PC))
	virtualMachine.scopes = append(virtualMachine.scopes, scope)
	bodyPC := toInt64Value(info["pc"])
	virtualMachine.PC = int(bodyPC)
	return nil
}

func toInt64Value(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	case *big.Int:
		if typed != nil && typed.IsInt64() {
			return typed.Int64()
		}
	}
	return 0
}

// ---------------------------------------------------------------------- #
// Contracts
// ---------------------------------------------------------------------- #

// DeployContract deploys a contract through the contract manager.
func (virtualMachine *VM) DeployContract(contractID string, bytecode []any, gasLimit *int64) error {
	return virtualMachine.ContractManager.DeployContract(contractID, bytecode, gasLimit)
}

// CallContractFunction calls a contract function through the manager.
func (virtualMachine *VM) CallContractFunction(contractID, functionName string,
	params []any, gasLimit *int64) (any, error) {
	return virtualMachine.ContractManager.CallContractFunction(contractID, functionName, params, gasLimit)
}
