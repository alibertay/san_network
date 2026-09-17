package ledger

import (
	"sort"
)

// Every node must produce the exact same genesis block, otherwise chains can
// never be synced or compared. Fixed timestamp keeps the genesis hash stable.
const (
	GenesisTimestamp         = 0.0
	GenesisMessage           = "TEXT A MESSAGE TO THE HUMANITY"
	DefaultChainID           = "san-devnet-1"
	DefaultProposerTimeoutMs = 6000
)

// GovernanceParameter bounds: parameters a validator-majority may change.
type GovernanceParameter struct {
	Min int64
	Max *int64
}

func int64Pointer(value int64) *int64 { return &value }

// GovernanceParameters lists the consensus parameters governance may change.
var GovernanceParameters = map[string]GovernanceParameter{
	"min_validator_stake":   {Min: 0, Max: nil},
	"unbonding_period":      {Min: 0, Max: nil},
	"slash_bps":             {Min: 0, Max: int64Pointer(10_000)},
	"block_gas_limit":       {Min: 100_000, Max: nil},
	"proposer_timeout_ms":   {Min: 100, Max: int64Pointer(60_000)},
	"block_reward":          {Min: 0, Max: nil},
	"min_block_interval_ms": {Min: 0, Max: int64Pointer(600_000)},
}

// GovernanceValueOK reports whether value is in range for a parameter.
func GovernanceValueOK(name string, value int64) bool {
	parameter, ok := GovernanceParameters[name]
	if !ok {
		return false
	}
	if value < parameter.Min {
		return false
	}
	return parameter.Max == nil || value <= *parameter.Max
}

// BlockchainConfig carries the genesis parameters.
type BlockchainConfig struct {
	GenesisBalances    map[string]int64
	ChainID            string
	MinValidatorStake  int64
	UnbondingPeriod    int64
	SlashBps           int64
	BlockGasLimit      int64
	ProposerTimeoutMs  int64
	BlockReward        int64
	MinBlockIntervalMs int64
	configured         bool
}

// DefaultBlockchainConfig mirrors the Python constructor defaults.
func DefaultBlockchainConfig() BlockchainConfig {
	return BlockchainConfig{
		ChainID:            DefaultChainID,
		MinValidatorStake:  MinValidatorStakeUnits,
		UnbondingPeriod:    DefaultUnbondingPeriod,
		SlashBps:           DefaultSlashBps,
		BlockGasLimit:      30_000_000,
		ProposerTimeoutMs:  DefaultProposerTimeoutMs,
		BlockReward:        0,
		MinBlockIntervalMs: 0,
		configured:         true,
	}
}

// Blockchain mirrors blockchain/Blockchain.py.
type Blockchain struct {
	ChainID            string
	Chain              []*Block
	SAN                map[string]int64
	GenesisAllocations map[string]int64
	Nonces             map[string]int64
	Validators         map[string]map[string]any
	TotalSlashed       int64
	TotalBurned        int64
	BaseFee            int64
	Parameters         map[string]int64
	GenesisParameters  map[string]int64
	GenesisStateRoot   string
}

// NewBlockchain builds the chain with its deterministic genesis block.
func NewBlockchain(config BlockchainConfig) *Blockchain {
	if !config.configured {
		config = DefaultBlockchainConfig()
	}
	if config.ChainID == "" {
		config.ChainID = DefaultChainID
	}

	balances := map[string]int64{}
	for address, units := range config.GenesisBalances {
		balances[address] = units
	}
	genesisAllocations := map[string]int64{}
	for address, units := range balances {
		genesisAllocations[address] = units
	}

	parameters := map[string]int64{
		"min_validator_stake":   config.MinValidatorStake,
		"unbonding_period":      config.UnbondingPeriod,
		"slash_bps":             config.SlashBps,
		"block_gas_limit":       config.BlockGasLimit,
		"proposer_timeout_ms":   config.ProposerTimeoutMs,
		"block_reward":          config.BlockReward,
		"min_block_interval_ms": config.MinBlockIntervalMs,
	}
	genesisParameters := map[string]int64{}
	for name, value := range parameters {
		genesisParameters[name] = value
	}

	chain := &Blockchain{
		ChainID:            config.ChainID,
		Chain:              []*Block{},
		SAN:                balances,
		GenesisAllocations: genesisAllocations,
		Nonces:             map[string]int64{},
		Validators:         map[string]map[string]any{},
		TotalSlashed:       0,
		TotalBurned:        0,
		BaseFee:            InitialBaseFee,
		Parameters:         parameters,
		GenesisParameters:  genesisParameters,
	}

	chain.GenesisStateRoot = StateRoot(StateInput{
		Balances:     chain.SAN,
		Nonces:       map[string]int64{},
		Validators:   map[string]map[string]any{},
		TotalSlashed: 0,
		Storage:      map[string]any{},
		Parameters:   chain.Parameters,
		BaseFee:      chain.BaseFee,
		TotalBurned:  0,
	})
	chain.createGenesisBlock()
	return chain
}

// MinValidatorStake reads the consensus parameter.
func (chain *Blockchain) MinValidatorStake() int64 { return chain.Parameters["min_validator_stake"] }

// UnbondingPeriod reads the consensus parameter.
func (chain *Blockchain) UnbondingPeriod() int64 { return chain.Parameters["unbonding_period"] }

// SlashBps reads the consensus parameter.
func (chain *Blockchain) SlashBps() int64 { return chain.Parameters["slash_bps"] }

// BlockGasLimit reads the consensus parameter.
func (chain *Blockchain) BlockGasLimit() int64 { return chain.Parameters["block_gas_limit"] }

// BlockReward reads the per-block subsidy.
func (chain *Blockchain) BlockReward() int64 { return chain.Parameters["block_reward"] }

// ProposerTimeout is the round deadline in seconds.
func (chain *Blockchain) ProposerTimeout() float64 {
	return float64(chain.Parameters["proposer_timeout_ms"]) / 1000.0
}

// MinBlockInterval is the consensus minimum seconds between blocks.
func (chain *Blockchain) MinBlockInterval() float64 {
	configured := float64(chain.Parameters["min_block_interval_ms"]) / 1000.0
	if chain.BlockReward() > 0 && configured < 1.0 {
		return 1.0
	}
	return configured
}

// ActiveValidators returns staked validators eligible at the current tip.
func (chain *Blockchain) ActiveValidators() map[string]int64 {
	tipIndex := chain.Tip().Index
	active := map[string]int64{}
	for address, info := range chain.Validators {
		if info["release_height"] != nil {
			continue
		}
		if toInt64(info["joined_height"]) > tipIndex {
			continue
		}
		stake := toInt64(info["stake"])
		if stake >= chain.MinValidatorStake() {
			active[address] = stake
		}
	}
	return active
}

// TotalActiveStake sums the active validator stakes.
func (chain *Blockchain) TotalActiveStake() int64 {
	total := int64(0)
	for _, stake := range chain.ActiveValidators() {
		total += stake
	}
	return total
}

// ActiveValidatorsFor is the active set used to weight votes at height.
func (chain *Blockchain) ActiveValidatorsFor(height int64) map[string]int64 {
	cutoff := height - 1
	if cutoff < 0 {
		cutoff = 0
	}
	active := map[string]int64{}
	for address, info := range chain.Validators {
		if toInt64(info["joined_height"]) > cutoff {
			continue
		}
		stake := toInt64(info["stake"])
		if stake >= chain.MinValidatorStake() {
			active[address] = stake
		}
	}
	return active
}

func (chain *Blockchain) createGenesisBlock() {
	genesis := NewBlock(
		0,
		"0",
		"GENESIS_VALIDATOR",
		"GENESIS_SIGNATURE",
		[]any{GenesisMessage},
		GenesisTimestamp,
		chain.ChainID,
		chain.GenesisStateRoot,
		0,
		nil,
	)
	chain.Chain = append(chain.Chain, genesis)
}

// AddBlock appends a new block to the chain.
func (chain *Blockchain) AddBlock(validator any, validatorSignature any, transactions []any) *Block {
	last := chain.Chain[len(chain.Chain)-1]
	block := NewBlock(
		last.Index+1,
		last.CurrentBlockHash,
		validator,
		validatorSignature,
		transactions,
		nil,
		chain.ChainID,
		nil,
		0,
		nil,
	)
	chain.Chain = append(chain.Chain, block)
	return block
}

// Tip returns the current chain tip.
func (chain *Blockchain) Tip() *Block {
	return chain.Chain[len(chain.Chain)-1]
}

// BlocksSince returns blocks with index >= fromIndex (optionally capped).
func (chain *Blockchain) BlocksSince(fromIndex int64, limit *int64) []*Block {
	if fromIndex < 0 {
		fromIndex = 0
	}
	first := int64(0)
	if len(chain.Chain) > 0 {
		first = chain.Chain[0].Index
	}
	offset := fromIndex - first
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(chain.Chain)) {
		offset = int64(len(chain.Chain))
	}
	if limit == nil {
		return append([]*Block{}, chain.Chain[offset:]...)
	}
	end := offset + *limit
	if end < offset {
		end = offset
	}
	if end > int64(len(chain.Chain)) {
		end = int64(len(chain.Chain))
	}
	return append([]*Block{}, chain.Chain[offset:end]...)
}

// NextFeeRate is the deterministic fee per byte for the next block.
func (chain *Blockchain) NextFeeRate() int64 {
	return FeeRateForTransactionCount(int64(len(chain.Tip().Transactions)))
}

// MedianTimePast is the median timestamp of the last window blocks.
func (chain *Blockchain) MedianTimePast(window int) float64 {
	if window <= 0 {
		window = 11
	}
	start := len(chain.Chain) - window
	if start < 0 {
		start = 0
	}
	timestamps := make([]float64, 0, len(chain.Chain)-start)
	for _, block := range chain.Chain[start:] {
		timestamps = append(timestamps, block.TimestampFloat())
	}
	if len(timestamps) == 0 {
		return 0.0
	}
	sort.Float64s(timestamps)
	middle := len(timestamps) / 2
	if len(timestamps)%2 == 1 {
		return timestamps[middle]
	}
	return (timestamps[middle-1] + timestamps[middle]) / 2.0
}

// StateSnapshot is the current ledger state as a JSON object.
func (chain *Blockchain) StateSnapshot() map[string]any {
	validators := map[string]any{}
	for address, info := range chain.Validators {
		copied := map[string]any{}
		for key, value := range info {
			copied[key] = value
		}
		validators[address] = copied
	}
	balances := map[string]any{}
	for address, units := range chain.SAN {
		balances[address] = units
	}
	nonces := map[string]any{}
	for address, nonce := range chain.Nonces {
		nonces[address] = nonce
	}
	parameters := map[string]any{}
	for name, value := range chain.Parameters {
		parameters[name] = value
	}
	return map[string]any{
		"balances":      balances,
		"nonces":        nonces,
		"validators":    validators,
		"total_slashed": chain.TotalSlashed,
		"total_burned":  chain.TotalBurned,
		"base_fee":      chain.BaseFee,
		"parameters":    parameters,
	}
}

// LoadState replaces the ledger state from a snapshot.
func (chain *Blockchain) LoadState(payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	balances := map[string]int64{}
	if raw, ok := payload["balances"].(map[string]any); ok {
		for address, units := range raw {
			balances[address] = toInt64(units)
		}
	}
	nonces := map[string]int64{}
	if raw, ok := payload["nonces"].(map[string]any); ok {
		for address, nonce := range raw {
			nonces[address] = toInt64(nonce)
		}
	}
	validators := map[string]map[string]any{}
	if raw, ok := payload["validators"].(map[string]any); ok {
		for address, info := range raw {
			if record, ok := info.(map[string]any); ok {
				copied := map[string]any{}
				for key, value := range record {
					copied[key] = value
				}
				validators[address] = copied
			}
		}
	}
	chain.SAN = balances
	chain.Nonces = nonces
	chain.Validators = validators
	chain.TotalSlashed = toInt64(payload["total_slashed"])
	chain.TotalBurned = toInt64(payload["total_burned"])
	baseFee := toInt64(payload["base_fee"])
	if baseFee == 0 {
		baseFee = InitialBaseFee
	}
	chain.BaseFee = baseFee
	if raw, ok := payload["parameters"].(map[string]any); ok {
		for key, value := range raw {
			chain.Parameters[key] = toInt64(value)
		}
	}
	if value, ok := chain.Parameters["proposer_timeout_ms"]; ok {
		chain.Parameters["proposer_timeout_ms"] = value
	}
}
