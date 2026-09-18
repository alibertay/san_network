package sdk

import "github.com/alibertay/san_network/internal/ledger"

// Stake returns the raw validator/stake registry record for an address. The
// returned map always carries "stake" (SAN) and "stake_units", plus
// "release_height" while an unbonding withdrawal is pending.
func (c *SanClient) Stake(address string) (map[string]any, error) {
	if address == "" {
		own, err := c.Address()
		if err != nil {
			return nil, err
		}
		address = own
	}
	normalized, err := ledger.NormalizeAddress(address)
	if err != nil {
		return nil, err
	}
	return c.getObject("/stake/" + normalized)
}
