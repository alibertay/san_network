package sanvm

import "fmt"

// Storage mirrors SANVM/Storage.py: contract variables, registered functions
// and deployed contracts.
type Storage struct {
	Data      map[string]any
	Functions map[string]map[string]any
	Contracts map[string]map[string]any
}

// NewStorage creates an empty storage.
func NewStorage() *Storage {
	return &Storage{
		Data:      map[string]any{},
		Functions: map[string]map[string]any{},
		Contracts: map[string]map[string]any{},
	}
}

// StorageKey converts a stack value into a storage key. Python dicts accept
// any hashable; PENA contracts use strings, which map one to one.
func StorageKey(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprintf("%v", value)
}

// SetVar stores a variable.
func (storage *Storage) SetVar(key any, value any) {
	storage.Data[StorageKey(key)] = value
}

// GetVar reads a variable (missing keys read as 0).
func (storage *Storage) GetVar(key any) any {
	if value, ok := storage.Data[StorageKey(key)]; ok {
		return value
	}
	return int64(0)
}

// DeleteVar removes a variable.
func (storage *Storage) DeleteVar(key any) {
	delete(storage.Data, StorageKey(key))
}

// HasVar reports whether a variable exists.
func (storage *Storage) HasVar(key any) bool {
	_, ok := storage.Data[StorageKey(key)]
	return ok
}

// ToDict returns a deep, JSON-serializable snapshot.
func (storage *Storage) ToDict() map[string]any {
	data := map[string]any{}
	for key, value := range storage.Data {
		data[key] = deepCopy(value)
	}
	functions := map[string]any{}
	for name, info := range storage.Functions {
		copied := map[string]any{}
		for key, value := range info {
			copied[key] = deepCopy(value)
		}
		functions[name] = copied
	}
	contracts := map[string]any{}
	for id, record := range storage.Contracts {
		copied := map[string]any{}
		for key, value := range record {
			copied[key] = deepCopy(value)
		}
		contracts[id] = copied
	}
	return map[string]any{"data": data, "functions": functions, "contracts": contracts}
}

// LoadFromDict replaces the contents in place (the VM keeps a reference).
func (storage *Storage) LoadFromDict(payload map[string]any) {
	storage.Data = map[string]any{}
	storage.Functions = map[string]map[string]any{}
	storage.Contracts = map[string]map[string]any{}
	if payload == nil {
		return
	}
	if raw, ok := payload["data"].(map[string]any); ok {
		for key, value := range raw {
			storage.Data[key] = deepCopy(value)
		}
	}
	if raw, ok := payload["functions"].(map[string]any); ok {
		for name, info := range raw {
			record, ok := info.(map[string]any)
			if !ok {
				continue
			}
			copied := map[string]any{}
			for key, value := range record {
				copied[key] = deepCopy(value)
			}
			storage.Functions[name] = copied
		}
	}
	if raw, ok := payload["contracts"].(map[string]any); ok {
		for id, info := range raw {
			record, ok := info.(map[string]any)
			if !ok {
				continue
			}
			copied := map[string]any{}
			for key, value := range record {
				copied[key] = deepCopy(value)
			}
			storage.Contracts[id] = copied
		}
	}
}

// StorageFromDict builds a storage from a snapshot.
func StorageFromDict(payload map[string]any) *Storage {
	storage := NewStorage()
	storage.LoadFromDict(payload)
	return storage
}
