package netnode

import "github.com/alibertay/san_network/internal/ledger"

// ValidatorInfo returns the raw validator registry record for an address
// (nil when the address has never staked). The returned map always includes
// the normalized "address" plus "registered", "stake_units" and "stake"
// fields so callers can reconcile stakes without the active-validator filter.
func (n *Node) ValidatorInfo(address string) (map[string]any, error) {
	normalized, err := ledger.NormalizeAddress(address)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	info, ok := n.blockchain.Validators[normalized]
	var copied map[string]any
	if ok {
		copied = map[string]any{"address": normalized, "registered": true}
		for key, value := range info {
			copied[key] = value
		}
	} else {
		copied = map[string]any{
			"address":        normalized,
			"registered":     false,
			"stake":          0,
			"stake_units":    int64(0),
			"joined_height":  nil,
			"release_height": nil,
		}
	}
	n.mu.Unlock()

	if units, ok := numericStakeUnits(copied["stake"]); ok {
		copied["stake_units"] = units
		copied["stake"] = ledger.UnitsToSAN(units)
	}
	return copied, nil
}

func numericStakeUnits(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case float64:
		return int64(typed), true
	default:
		return 0, false
	}
}
