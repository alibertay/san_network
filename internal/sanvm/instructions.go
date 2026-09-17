package sanvm

import "fmt"

// ParseInstructionList mirrors utils/parser.py: it converts the legacy
// instruction-list transaction format ([["PUSH", 1], ["HALT"]]) into bytecode.
func ParseInstructionList(instructions []any) ([]any, error) {
	bytecode := []any{}
	for _, raw := range instructions {
		entry, ok := raw.([]any)
		if !ok || len(entry) == 0 {
			return nil, fmt.Errorf("Invalid instruction: %v", raw)
		}
		name, ok := entry[0].(string)
		if !ok {
			return nil, fmt.Errorf("Invalid instruction: %v", raw)
		}
		value, known := Mnemonics[name]
		if !known {
			return nil, fmt.Errorf("Unvalid opcode: %s", name)
		}
		bytecode = append(bytecode, int(value))
		if name == "PUSH" {
			if len(entry) < 2 {
				return nil, fmt.Errorf("PUSH without operand")
			}
			bytecode = append(bytecode, entry[1])
		}
	}
	return bytecode, nil
}
