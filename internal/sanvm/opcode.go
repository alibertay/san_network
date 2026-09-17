package sanvm

// OpCode is the SANVM instruction set (mirrors SANVM/OpCode.py).
type OpCode int

const (
	OpPush       OpCode = 0x01
	OpPop        OpCode = 0x02
	OpAdd        OpCode = 0x03
	OpSub        OpCode = 0x04
	OpMul        OpCode = 0x05
	OpDiv        OpCode = 0x06
	OpPrint      OpCode = 0x07
	OpMod        OpCode = 0x08
	OpJmp        OpCode = 0x09
	OpJz         OpCode = 0x10
	OpDup        OpCode = 0x0B
	OpSwap       OpCode = 0x0C
	OpAnd        OpCode = 0x0D
	OpOr         OpCode = 0x0E
	OpXor        OpCode = 0x0F
	OpEq         OpCode = 0x11
	OpNeq        OpCode = 0x12
	OpLt         OpCode = 0x13
	OpLte        OpCode = 0x14
	OpGt         OpCode = 0x15
	OpGte        OpCode = 0x16
	OpCall       OpCode = 0x17
	OpRet        OpCode = 0x18
	OpNop        OpCode = 0x19
	OpDrop       OpCode = 0x1A
	OpOver       OpCode = 0x1B
	OpRot        OpCode = 0x1C
	OpSet        OpCode = 0x1D
	OpGet        OpCode = 0x1E
	OpDelete     OpCode = 0x1F
	OpHas        OpCode = 0x20
	OpListAppend OpCode = 0x21
	OpListRemove OpCode = 0x22
	OpListLen    OpCode = 0x23
	OpListGet    OpCode = 0x24
	OpDictSet    OpCode = 0x25
	OpDictGet    OpCode = 0x26
	OpDictKeys   OpCode = 0x27
	OpDefFunc    OpCode = 0x2B
	OpCallFunc   OpCode = 0x2C
	OpEndFunc    OpCode = 0x2D
	OpJnz        OpCode = 0x2E
	OpHalt       OpCode = 0xFF
)

// OpNames maps opcode values to their mnemonics.
var OpNames = map[int]string{
	int(OpPush): "PUSH", int(OpPop): "POP", int(OpAdd): "ADD", int(OpSub): "SUB",
	int(OpMul): "MUL", int(OpDiv): "DIV", int(OpPrint): "PRINT", int(OpMod): "MOD",
	int(OpJmp): "JMP", int(OpJz): "JZ", int(OpDup): "DUP", int(OpSwap): "SWAP",
	int(OpAnd): "AND", int(OpOr): "OR", int(OpXor): "XOR", int(OpEq): "EQ",
	int(OpNeq): "NEQ", int(OpLt): "LT", int(OpLte): "LTE", int(OpGt): "GT",
	int(OpGte): "GTE", int(OpCall): "CALL", int(OpRet): "RET", int(OpNop): "NOP",
	int(OpDrop): "DROP", int(OpOver): "OVER", int(OpRot): "ROT", int(OpSet): "SET",
	int(OpGet): "GET", int(OpDelete): "DELETE", int(OpHas): "HAS",
	int(OpListAppend): "LIST_APPEND", int(OpListRemove): "LIST_REMOVE",
	int(OpListLen): "LIST_LEN", int(OpListGet): "LIST_GET",
	int(OpDictSet): "DICT_SET", int(OpDictGet): "DICT_GET", int(OpDictKeys): "DICT_KEYS",
	int(OpDefFunc): "DEF_FUNC", int(OpCallFunc): "CALL_FUNC", int(OpEndFunc): "END_FUNC",
	int(OpJnz): "JNZ", int(OpHalt): "HALT",
}

// Mnemonics maps mnemonic names to opcode values.
var Mnemonics = func() map[string]int {
	result := make(map[string]int, len(OpNames))
	for value, name := range OpNames {
		result[name] = value
	}
	return result
}()

// TargetMnemonics are the instructions whose operand is an address.
var TargetMnemonics = map[string]bool{
	"JMP": true, "JZ": true, "JNZ": true, "DEF_FUNC": true,
}

// VarMacros maps variable macros to the number of pending stack operands the
// opcode consumes (including the inserted variable name).
var VarMacros = map[string]int{
	"GET": 1, "HAS": 1, "DELETE": 1, "LIST_LEN": 1, "DICT_KEYS": 1,
	"SET": 2, "LIST_APPEND": 2, "LIST_REMOVE": 2, "LIST_GET": 2, "DICT_GET": 2,
	"DICT_SET": 3,
}
