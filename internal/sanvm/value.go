// Package sanvm is the Go port of SANVM: PENA assembly, the high-level PENA
// compiler, the stack-based virtual machine, contract storage and the contract
// manager. Values mirror Python's untyped model: int64 / *big.Int for
// integers, float64, string, []any and map[string]any.
package sanvm

import (
	"fmt"
	"math"
	"math/big"
	"strings"
)

// MaxIntBits caps integers on the stack (audit fix parity with Python).
const MaxIntBits = 4096

// TypeError marks an operand type failure. Python raises TypeError from the
// underlying operators; Go mirrors the error name so Go/Python error
// classification stays identical (section 15 parity).
type TypeError struct{ Message string }

func (e *TypeError) Error() string { return e.Message }

// repetitionCount converts a stack value into a Python slice repetition count.
func repetitionCount(value any) (int64, bool) {
	number, ok := toBig(value)
	if !ok || !number.IsInt64() {
		return 0, false
	}
	return number.Int64(), true
}

// repeatString mirrors Python's str * int (negative counts yield "").
func repeatString(text string, count int64) (any, error) {
	if count <= 0 {
		return "", nil
	}
	if int64(len(text))*count > MaxValueBytes {
		return nil, &VMError{Message: fmt.Sprintf("Value exceeds %d bytes", MaxValueBytes)}
	}
	return strings.Repeat(text, int(count)), nil
}

// repeatList mirrors Python's list * int (negative counts yield []).
func repeatList(list []any, count int64) (any, error) {
	if count <= 0 {
		return []any{}, nil
	}
	if int64(len(list))*count > MaxValueBytes {
		return nil, &VMError{Message: fmt.Sprintf("Value exceeds %d bytes", MaxValueBytes)}
	}
	repeated := make([]any, 0, int(int64(len(list))*count))
	for i := int64(0); i < count; i++ {
		repeated = append(repeated, list...)
	}
	return repeated, nil
}

// deepCopy mirrors copy.deepcopy for container values.
func deepCopy(value any) any {
	switch typed := value.(type) {
	case []any:
		copied := make([]any, len(typed))
		for i, item := range typed {
			copied[i] = deepCopy(item)
		}
		return copied
	case map[string]any:
		copied := make(map[string]any, len(typed))
		for key, item := range typed {
			copied[key] = deepCopy(item)
		}
		return copied
	case *big.Int:
		return new(big.Int).Set(typed)
	default:
		return value
	}
}

func isInteger(value any) bool {
	switch typed := value.(type) {
	case int, int8, int16, int32, int64, uint, uint64:
		return true
	case bool:
		return true
	case *big.Int:
		return typed != nil
	default:
		return false
	}
}

func toBig(value any) (*big.Int, bool) {
	switch typed := value.(type) {
	case bool:
		if typed {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	case int:
		return big.NewInt(int64(typed)), true
	case int8:
		return big.NewInt(int64(typed)), true
	case int16:
		return big.NewInt(int64(typed)), true
	case int32:
		return big.NewInt(int64(typed)), true
	case int64:
		return big.NewInt(typed), true
	case uint:
		return new(big.Int).SetUint64(uint64(typed)), true
	case uint64:
		return new(big.Int).SetUint64(typed), true
	case *big.Int:
		if typed == nil {
			return big.NewInt(0), true
		}
		return typed, true
	default:
		return nil, false
	}
}

func toFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case bool:
		if typed {
			return 1, true
		}
		return 0, true
	case *big.Int:
		if typed == nil {
			return 0, true
		}
		floatValue, _ := new(big.Float).SetInt(typed).Float64()
		return floatValue, true
	default:
		return 0, false
	}
}

func intBitLength(value any) int {
	bigValue, ok := toBig(value)
	if !ok {
		return 0
	}
	return bigValue.BitLen()
}

func normalizeInt(value *big.Int) any {
	if value.IsInt64() {
		return value.Int64()
	}
	return value
}

// truthy mirrors Python truthiness (0, "", [], {} and None are false).
func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case uint64:
		return typed != 0
	case float64:
		return typed != 0
	case *big.Int:
		return typed != nil && typed.Sign() != 0
	default:
		return true
	}
}

// isEmptyValue mirrors SANVM._is_empty: 0, None, "" and empty containers are
// all "empty".
func isEmptyValue(value any) bool {
	switch typed := value.(type) {
	case []any, map[string]any, string:
		return !truthy(typed)
	case nil:
		return true
	}
	if number, ok := toFloat(value); ok {
		return number == 0
	}
	return false
}

func valuesEqual(a, b any) bool {
	if bothNumeric(a, b) {
		return compareNumbers(a, b) == 0
	}
	switch left := a.(type) {
	case string:
		right, ok := b.(string)
		return ok && left == right
	case []any:
		right, ok := b.([]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for i := range left {
			if !valuesEqual(left[i], right[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		right, ok := b.(map[string]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for key, item := range left {
			other, ok := right[key]
			if !ok || !valuesEqual(item, other) {
				return false
			}
		}
		return true
	case nil:
		return b == nil
	default:
		return false
	}
}

func bothNumeric(a, b any) bool {
	return (isInteger(a) || isFloat(a)) && (isInteger(b) || isFloat(b))
}

func isFloat(value any) bool {
	switch value.(type) {
	case float64, float32:
		return true
	default:
		return false
	}
}

// compareNumbers returns -1/0/1 for two numeric values (exact for ints).
func compareNumbers(a, b any) int {
	if !isFloat(a) && !isFloat(b) {
		left, leftOK := toBig(a)
		right, rightOK := toBig(b)
		if !leftOK || !rightOK {
			// Defensive: mixed or unsupported values are unordered, never a
			// nil-pointer dereference (fuzzer regression).
			return 2
		}
		return left.Cmp(right)
	}
	left, _ := toFloat(a)
	right, _ := toFloat(b)
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	case left == right:
		return 0
	case math.IsNaN(left) || math.IsNaN(right):
		return 2 // unordered
	default:
		return 2
	}
}

func pythonModInt(a, b *big.Int) (*big.Int, error) {
	if b.Sign() == 0 {
		return nil, &VMError{Message: "Modulo by zero"}
	}
	remainder := new(big.Int)
	quotient := new(big.Int)
	quotient.QuoRem(a, b, remainder)
	if remainder.Sign() != 0 && remainder.Sign() != b.Sign() {
		remainder.Add(remainder, b)
	}
	return remainder, nil
}

func pythonFloorDivInt(a, b *big.Int) (*big.Int, error) {
	if b.Sign() == 0 {
		return nil, &VMError{Message: "Division by zero"}
	}
	quotient := new(big.Int)
	remainder := new(big.Int)
	quotient.QuoRem(a, b, remainder)
	if remainder.Sign() != 0 && remainder.Sign() != b.Sign() {
		quotient.Sub(quotient, big.NewInt(1))
	}
	return quotient, nil
}

func pythonModFloat(a, b float64) (float64, error) {
	if b == 0 {
		return 0, &VMError{Message: "Modulo by zero"}
	}
	remainder := math.Mod(a, b)
	if remainder != 0 && (remainder < 0) != (b < 0) {
		remainder += b
	}
	return remainder, nil
}

func formatFloat(value float64) string {
	if value == math.Trunc(value) && !math.IsInf(value, 0) && math.Abs(value) < 1e16 {
		return fmt.Sprintf("%.1f", value)
	}
	return fmt.Sprintf("%g", value)
}
