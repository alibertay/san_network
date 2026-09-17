package sanvm

import "fmt"

// ContractManager mirrors SANVM/ContractManager.py.
type ContractManager struct {
	Storage     *Storage
	LastGasUsed int64
	LastLogs    []map[string]any
}

// NewContractManager builds a manager over a storage.
func NewContractManager(storage *Storage) *ContractManager {
	if storage == nil {
		storage = NewStorage()
	}
	return &ContractManager{Storage: storage, LastLogs: []map[string]any{}}
}

// Is256BitOrSmallerStr reports whether the decimal string fits in 256 bits.
func Is256BitOrSmallerStr(value string) bool {
	const max256 = "115792089237316195423570985008687907853269984665640564039457584007913129639936"
	trimmed := value
	for len(trimmed) > 0 && trimmed[0] == '0' {
		trimmed = trimmed[1:]
	}
	if trimmed == "" {
		return true
	}
	if len(trimmed) != len(max256) {
		return len(trimmed) < len(max256)
	}
	return trimmed <= max256
}

// DeployContract runs the contract's top-level code and registers it.
func (manager *ContractManager) DeployContract(contractID string, bytecode []any, gasLimit *int64) error {
	if contractID == "" {
		return fmt.Errorf("Invalid contract id: %q", contractID)
	}
	if !Is256BitOrSmallerStr(contractID) {
		return fmt.Errorf("Invalid contract id: %q", contractID)
	}
	if _, exists := manager.Storage.Contracts[contractID]; exists {
		return fmt.Errorf("Contract %q already exists", contractID)
	}

	contractStorage := NewStorage()
	virtualMachine := NewVM(contractStorage)
	if err := virtualMachine.Run(bytecode, 0, gasLimit); err != nil {
		return err
	}
	manager.LastGasUsed = virtualMachine.GasUsed
	manager.LastLogs = append([]map[string]any{}, virtualMachine.Logs...)

	manager.Storage.Contracts[contractID] = map[string]any{
		"bytecode": append([]any{}, bytecode...),
		"storage":  contractStorage.ToDict(),
	}
	return nil
}

// CallContractFunction runs a function on an isolated storage snapshot and
// commits it only if the call completes successfully (atomic call).
func (manager *ContractManager) CallContractFunction(contractID, functionName string,
	args []any, gasLimit *int64) (any, error) {
	contract, ok := manager.Storage.Contracts[contractID]
	if !ok {
		return nil, fmt.Errorf("%s is not a valid contract", contractID)
	}
	contractBytecode, _ := contract["bytecode"].([]any)
	contractStorage := StorageFromDict(asMap(contract["storage"]))

	virtualMachine := NewVM(contractStorage)

	callBytecode := append([]any{}, contractBytecode...)
	for _, arg := range args {
		callBytecode = append(callBytecode, int(OpPush), arg)
	}
	callBytecode = append(callBytecode,
		int(OpPush), functionName, int(OpPush), int64(len(args)), int(OpCallFunc))

	if err := virtualMachine.Run(callBytecode, len(contractBytecode), gasLimit); err != nil {
		return nil, err
	}
	manager.LastGasUsed = virtualMachine.GasUsed
	manager.LastLogs = append([]map[string]any{}, virtualMachine.Logs...)

	var returnValue any
	if len(virtualMachine.Stack) > 0 {
		returnValue = virtualMachine.Stack[len(virtualMachine.Stack)-1]
	}
	contract["storage"] = contractStorage.ToDict()
	return returnValue, nil
}

func asMap(value any) map[string]any {
	if result, ok := value.(map[string]any); ok {
		return result
	}
	return nil
}
