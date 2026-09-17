package sanvm

import (
	"fmt"

	"github.com/alibertay/san_network/internal/canonical"
)

// Deterministic gas schedule. Every opcode has a fixed cost so that a
// transaction's gas usage is identical on every node.
const (
	DefaultCost            = 1
	PushPayloadBytesPerGas = 16
)

// GasCosts mirrors SANVM/gas.py.
var GasCosts = map[int]int{
	int(OpPush): 1, int(OpPop): 1, int(OpDrop): 1,
	int(OpAdd): 1, int(OpSub): 1, int(OpMul): 2, int(OpDiv): 3, int(OpMod): 3,
	int(OpPrint): 10,
	int(OpJmp):   2, int(OpJz): 2, int(OpJnz): 2, int(OpNop): 0, int(OpHalt): 0,
	int(OpDup): 1, int(OpSwap): 1, int(OpOver): 1, int(OpRot): 1,
	int(OpAnd): 1, int(OpOr): 1, int(OpXor): 1,
	int(OpEq): 1, int(OpNeq): 1, int(OpLt): 1, int(OpLte): 1, int(OpGt): 1, int(OpGte): 1,
	int(OpCall): 5, int(OpRet): 5, int(OpEndFunc): 5, int(OpDefFunc): 5,
	int(OpCallFunc): 10,
	// State access
	int(OpSet): 20, int(OpGet): 10, int(OpDelete): 20, int(OpHas): 10,
	int(OpListAppend): 25, int(OpListRemove): 25, int(OpListLen): 10, int(OpListGet): 10,
	int(OpDictSet): 25, int(OpDictGet): 10, int(OpDictKeys): 15,
}

// InstructionCost returns the fixed gas cost for an opcode.
func InstructionCost(opcode int) int {
	if cost, ok := GasCosts[opcode]; ok {
		return cost
	}
	return DefaultCost
}

// PayloadCost charges PUSH payloads by canonical serialized size.
func PayloadCost(value any) int {
	size := 0
	encoded, err := canonical.Marshal(value)
	if err != nil {
		size = len(fmt.Sprintf("%v", value))
	} else {
		size = len(encoded)
	}
	return size / PushPayloadBytesPerGas
}
