package ledger

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/alibertay/san_network/internal/canonical"
)

// SAN money model. All amounts are integer base units (like wei) so balances
// never drift because of floating point rounding.
const (
	SANDecimals = 8
	SANBase     = int64(100_000_000) // 1 SAN = 100_000_000 units

	MinFeePerByte        = SANBase / 100 // 0.01 SAN per byte
	MaxFeePerByte        = SANBase / 10  // 0.1 SAN per byte
	CongestionTxPerStep  = 100
	CongestionMaxSteps   = 10
	MinGasPrice          = int64(1)
	DefaultBlockGasLimit = int64(30_000_000)

	MinValidatorStakeUnits = int64(1000) * SANBase
	DefaultUnbondingPeriod = 100
	DefaultSlashBps        = 5000
	FinalityNumerator      = 2
	FinalityDenominator    = 3
	InitialBaseFee         = int64(1)
	BaseFeeMaxChangeDenom  = 8
	BaseFeeTargetDivisor   = 2
)

// NextBaseFee is the deterministic base-fee adjustment for the next block
// (EIP-1559 style).
func NextBaseFee(currentBaseFee, gasUsed, gasLimit int64) int64 {
	current := currentBaseFee
	if current < InitialBaseFee {
		current = InitialBaseFee
	}
	target := gasLimit / BaseFeeTargetDivisor
	if target < 1 {
		target = 1
	}
	used := gasUsed
	if used < 0 {
		used = 0
	}
	if used == target {
		return current
	}
	delta := used - target
	if delta < 0 {
		delta = -delta
	}
	change := current * delta / target / BaseFeeMaxChangeDenom
	if change < 1 {
		change = 1
	}
	if used > target {
		return current + change
	}
	next := current - change
	if next < InitialBaseFee {
		next = InitialBaseFee
	}
	return next
}

// SanToUnits converts a SAN amount (int/float/string/*big.Int) into integer
// base units, rejecting amounts with more than 8 decimals.
func SanToUnits(amount any) (int64, error) {
	switch value := amount.(type) {
	case nil:
		return 0, fmt.Errorf("Invalid SAN amount: %v", amount)
	case bool:
		return 0, fmt.Errorf("Invalid SAN amount: %v", amount)
	case int:
		return parseAmount(strconv.FormatInt(int64(value), 10), amount)
	case int8:
		return parseAmount(strconv.FormatInt(int64(value), 10), amount)
	case int16:
		return parseAmount(strconv.FormatInt(int64(value), 10), amount)
	case int32:
		return parseAmount(strconv.FormatInt(int64(value), 10), amount)
	case int64:
		return parseAmount(strconv.FormatInt(value, 10), amount)
	case uint:
		return parseAmount(strconv.FormatUint(uint64(value), 10), amount)
	case uint64:
		return parseAmount(strconv.FormatUint(value, 10), amount)
	case *big.Int:
		if value == nil {
			return 0, fmt.Errorf("Invalid SAN amount: %v", amount)
		}
		return parseAmount(value.String(), amount)
	case float64:
		return parseAmount(canonicalFloatString(value), amount)
	case float32:
		return parseAmount(canonicalFloatString(float64(value)), amount)
	case string:
		return parseAmount(strings.TrimSpace(value), amount)
	default:
		return 0, fmt.Errorf("Invalid SAN amount: %v", amount)
	}
}

func canonicalFloatString(value float64) string {
	text, err := canonical.MarshalString(value)
	if err != nil {
		return strconv.FormatFloat(value, 'g', -1, 64)
	}
	return text
}

func parseAmount(text string, original any) (int64, error) {
	if text == "" {
		return 0, fmt.Errorf("Invalid SAN amount: %v", original)
	}
	if strings.ContainsAny(text, "nNiIfF") {
		lower := strings.ToLower(text)
		if strings.Contains(lower, "nan") || strings.Contains(lower, "inf") {
			return 0, fmt.Errorf("Invalid SAN amount: %v", original)
		}
	}
	rational, ok := new(big.Rat).SetString(text)
	if !ok {
		return 0, fmt.Errorf("Invalid SAN amount: %v", original)
	}
	scaled := new(big.Rat).Mul(rational, big.NewRat(SANBase, 1))
	if !scaled.IsInt() {
		return 0, fmt.Errorf("Amount has more than %d decimals: %v", SANDecimals, original)
	}
	units := scaled.Num()
	if !units.IsInt64() {
		return 0, fmt.Errorf("Invalid SAN amount: %v", original)
	}
	return units.Int64(), nil
}

// UnitsToSAN renders units the way Python's str(Decimal(units) / SAN_BASE)
// does, e.g. 1 -> "1E-8", 100000000 -> "1".
func UnitsToSAN(units int64) string {
	if units == 0 {
		return "0"
	}
	negative := units < 0
	if negative {
		units = -units
	}
	digits := strconv.FormatInt(units, 10)
	exponent := -SANDecimals
	for exponent < 0 && strings.HasSuffix(digits, "0") {
		digits = digits[:len(digits)-1]
		exponent++
	}

	adjusted := exponent + len(digits) - 1
	var out string
	switch {
	case exponent <= 0 && adjusted >= -6:
		if exponent == 0 {
			out = digits
		} else if len(digits)+exponent <= 0 {
			out = "0." + strings.Repeat("0", -exponent-len(digits)) + digits
		} else {
			split := len(digits) + exponent
			out = digits[:split] + "." + digits[split:]
		}
	case exponent <= 0:
		if len(digits) == 1 {
			out = digits
		} else {
			out = digits[:1] + "." + digits[1:]
		}
		out += "E" + strconv.Itoa(adjusted)
	default:
		out = digits + strings.Repeat("0", exponent)
	}

	if negative {
		out = "-" + out
	}
	return out
}

// FeeRateForTransactionCount is the fee per byte (in base units) for a block
// whose parent has N transactions.
func FeeRateForTransactionCount(transactionCount int64) int64 {
	if transactionCount < 0 {
		transactionCount = 0
	}
	steps := transactionCount / CongestionTxPerStep
	if steps > CongestionMaxSteps {
		steps = CongestionMaxSteps
	}
	rate := MinFeePerByte * (1 + steps)
	if rate > MaxFeePerByte {
		rate = MaxFeePerByte
	}
	return rate
}
