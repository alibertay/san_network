package ledger

import (
	"math/rand"
	"reflect"
	"testing"

	"github.com/alibertay/san_network/internal/canonical"
)

// ---------------------------------------------------------------------- #
// Determinism (section 1): every consensus-critical map iteration is ordered.
// ---------------------------------------------------------------------- #

func shuffledIntMap(rng *rand.Rand, input map[string]int64) map[string]int64 {
	result := map[string]int64{}
	for key, value := range input {
		result[key] = value
	}
	keys := make([]string, 0, len(result))
	for key := range result {
		keys = append(keys, key)
	}
	rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	rebuilt := map[string]int64{}
	for _, key := range keys {
		rebuilt[key] = result[key]
	}
	return rebuilt
}

// TestActiveValidatorSetsDeterministic proves ActiveValidators and
// ActiveValidatorsFor return the same set and total stake over repeated calls
// with many validators, independent of Go's random map iteration order.
func TestActiveValidatorSetsDeterministic(t *testing.T) {
	config := DefaultBlockchainConfig()
	config.MinValidatorStake = 100
	chain := NewBlockchain(config)

	rng := rand.New(rand.NewSource(20240918))
	for i := 0; i < 40; i++ {
		address := "0x" + randHex(rng, 40)
		info := map[string]any{
			"public_key":     address,
			"stake":          int64(100 + i),
			"joined_height":  int64(i % 7),
			"release_height": nil,
		}
		if i%5 == 0 {
			info["release_height"] = int64(50)
		}
		if i%9 == 0 {
			info["stake"] = int64(10) // below min_validator_stake
		}
		chain.Validators[address] = info
	}

	baseline := sortedInt6Value(chain.ActiveValidators())
	baselineFor := sortedInt6Value(chain.ActiveValidatorsFor(5))
	for run := 0; run < 100; run++ {
		if got := sortedInt6Value(chain.ActiveValidators()); !reflect.DeepEqual(got, baseline) {
			t.Fatalf("ActiveValidators changed between runs:\n got %v\nwant %v", got, baseline)
		}
		if got := sortedInt6Value(chain.ActiveValidatorsFor(5)); !reflect.DeepEqual(got, baselineFor) {
			t.Fatalf("ActiveValidatorsFor changed between runs:\n got %v\nwant %v", got, baselineFor)
		}
	}
	if len(baseline) == 0 {
		t.Fatalf("test setup produced no active validators")
	}
}

func sortedInt6Value(input map[string]int64) map[string]int64 {
	result := map[string]int64{}
	for key, value := range input {
		result[key] = value
	}
	return result
}

func randHex(rng *rand.Rand, length int) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, length)
	for i := range out {
		out[i] = hexDigits[rng.Intn(len(hexDigits))]
	}
	return string(out)
}

// TestStateRootDeterministicAcrossInsertionOrder rebuilds the same state in
// shuffled orders and asserts the Merkle root never changes.
func TestStateRootDeterministicAcrossInsertionOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	balances := map[string]int64{}
	nonces := map[string]int64{}
	validators := map[string]map[string]any{}
	for i := 0; i < 30; i++ {
		address := "0x" + randHex(rng, 40)
		balances[address] = rng.Int63n(1_000_000)
		nonces[address] = int64(i)
		validators[address] = map[string]any{
			"public_key":     address,
			"stake":          int64(i * 10),
			"joined_height":  int64(i % 3),
			"release_height": nil,
		}
	}
	storage := map[string]any{
		"contracts": map[string]any{
			"zeta":  map[string]any{"bytecode": []any{int64(1)}, "storage": map[string]any{"x": int64(1)}},
			"alpha": map[string]any{"bytecode": []any{int64(2)}, "storage": map[string]any{"y": int64(2)}},
		},
		"data":      map[string]any{"b": int64(2), "a": int64(1)},
		"functions": map[string]any{"f": int64(1)},
	}
	parameters := map[string]int64{"min_validator_stake": 1, "slash_bps": 5000, "block_reward": 2}

	baseline := StateRoot(StateInput{
		Balances: balances, Nonces: nonces, Validators: validators,
		TotalSlashed: 5, Storage: storage, Parameters: parameters, BaseFee: 3, TotalBurned: 9,
	})
	for run := 0; run < 100; run++ {
		root := StateRoot(StateInput{
			Balances: shuffledIntMap(rng, balances), Nonces: shuffledIntMap(rng, nonces),
			Validators:   shuffledValidators(rng, validators),
			TotalSlashed: 5, Storage: storage, Parameters: parameters, BaseFee: 3, TotalBurned: 9,
		})
		if root != baseline {
			t.Fatalf("state root changed between runs: got %s, want %s", root, baseline)
		}
	}
}

func shuffledValidators(rng *rand.Rand, input map[string]map[string]any) map[string]map[string]any {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	result := map[string]map[string]any{}
	for _, key := range keys {
		result[key] = input[key]
	}
	return result
}

// TestStateSnapshotRoundTripDeterministic serializes and reloads the full
// ledger state repeatedly and asserts the root is stable.
func TestStateSnapshotRoundTripDeterministic(t *testing.T) {
	config := DefaultBlockchainConfig()
	config.GenesisBalances = map[string]int64{"0xaaaa": 100, "0xbbbb": 200}
	config.MinValidatorStake = 10
	chain := NewBlockchain(config)
	chain.Validators = map[string]map[string]any{
		"0xaaaa": {"public_key": "0xaaaa", "stake": int64(50), "joined_height": int64(0), "release_height": nil},
		"0xbbbb": {"public_key": "0xbbbb", "stake": int64(60), "joined_height": int64(1), "release_height": nil},
	}
	chain.Nonces["0xaaaa"] = 3
	chain.TotalSlashed = 7
	chain.TotalBurned = 11
	chain.BaseFee = 5
	chain.Parameters["block_reward"] = 2

	snapshot := chain.StateSnapshot()
	encoded, err := canonical.Marshal(snapshot)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	baseline := StateRoot(StateInput{
		Balances: chain.SAN, Nonces: chain.Nonces, Validators: chain.Validators,
		TotalSlashed: chain.TotalSlashed, Storage: map[string]any{}, Parameters: chain.Parameters,
		BaseFee: chain.BaseFee, TotalBurned: chain.TotalBurned,
	})
	for run := 0; run < 100; run++ {
		decoded, err := canonical.Decode(encoded)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		payload, ok := decoded.(map[string]any)
		if !ok {
			t.Fatalf("decoded snapshot is not an object")
		}
		restored := NewBlockchain(config)
		restored.LoadState(payload)
		root := StateRoot(StateInput{
			Balances: restored.SAN, Nonces: restored.Nonces, Validators: restored.Validators,
			TotalSlashed: restored.TotalSlashed, Storage: map[string]any{}, Parameters: restored.Parameters,
			BaseFee: restored.BaseFee, TotalBurned: restored.TotalBurned,
		})
		if root != baseline {
			t.Fatalf("snapshot round trip changed the state root: got %s, want %s", root, baseline)
		}
	}
}

// TestStateEntriesOrdered pins the ordering of account keys and contract keys.
func TestStateEntriesOrdered(t *testing.T) {
	input := StateInput{
		Balances: map[string]int64{"0xcccc": 1, "0xaaaa": 1, "0xbbbb": 1},
		Storage: map[string]any{"contracts": map[string]any{
			"z": map[string]any{"bytecode": []any{}, "storage": map[string]any{}},
			"a": map[string]any{"bytecode": []any{}, "storage": map[string]any{}},
		}},
		Parameters: map[string]int64{"x": 1},
	}
	keys := []string{}
	for _, entry := range StateEntries(input) {
		keys = append(keys, entry.(map[string]any)["key"].(string))
	}
	want := []string{"acct:0xaaaa", "acct:0xbbbb", "acct:0xcccc", "code:a", "store:a", "code:z", "store:z", "vmdata", "vmfuncs", "total_slashed", "total_burned", "base_fee", "parameters"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("state entry order:\n got %v\nwant %v", keys, want)
	}
}

// ---------------------------------------------------------------------- #
// Validator set transitions
// ---------------------------------------------------------------------- #

// TestActiveValidatorHeightCutoff pins the height-1 cutoff and the deliberate
// release-timing exemption of ActiveValidatorsFor.
func TestActiveValidatorHeightCutoff(t *testing.T) {
	config := DefaultBlockchainConfig()
	config.MinValidatorStake = 100
	chain := NewBlockchain(config)
	chain.Validators = map[string]map[string]any{
		"0xaaaa": {"public_key": "0xaaaa", "stake": int64(1000), "joined_height": int64(0), "release_height": nil},
		"0xbbbb": {"public_key": "0xbbbb", "stake": int64(1000), "joined_height": int64(5), "release_height": nil},
		"0xcccc": {"public_key": "0xcccc", "stake": int64(1000), "joined_height": int64(0), "release_height": int64(3)},
		"0xdddd": {"public_key": "0xdddd", "stake": int64(99), "joined_height": int64(0), "release_height": nil},
	}

	if _, present := chain.ActiveValidators()["0xbbbb"]; present {
		t.Errorf("validator joined at height 5 must not be active at the genesis tip")
	}
	if _, present := chain.ActiveValidators()["0xcccc"]; present {
		t.Errorf("undelegating validator must not be in ActiveValidators")
	}
	if _, present := chain.ActiveValidators()["0xdddd"]; present {
		t.Errorf("validator below min_validator_stake must not be active")
	}

	setAtFive := chain.ActiveValidatorsFor(5)
	if _, present := setAtFive["0xbbbb"]; present {
		t.Errorf("joined_height 5 must be excluded from ActiveValidatorsFor(5) (cutoff 4)")
	}
	if _, present := setAtFive["0xcccc"]; !present {
		t.Errorf("ActiveValidatorsFor must ignore release timing")
	}
	if _, present := chain.ActiveValidatorsFor(6)["0xbbbb"]; !present {
		t.Errorf("joined_height 5 must be included from ActiveValidatorsFor(6) on")
	}
}

// ---------------------------------------------------------------------- #
// Governance value bounds
// ---------------------------------------------------------------------- #

func TestGovernanceValueBounds(t *testing.T) {
	cases := []struct {
		name  string
		value int64
		ok    bool
	}{
		{"min_validator_stake", 0, true},
		{"min_validator_stake", -1, false},
		{"unbonding_period", 0, true},
		{"unbonding_period", -1, false},
		{"slash_bps", 0, true},
		{"slash_bps", 10_000, true},
		{"slash_bps", 10_001, false},
		{"slash_bps", -1, false},
		{"block_gas_limit", 100_000, true},
		{"block_gas_limit", 99_999, false},
		{"proposer_timeout_ms", 100, true},
		{"proposer_timeout_ms", 60_000, true},
		{"proposer_timeout_ms", 99, false},
		{"proposer_timeout_ms", 60_001, false},
		{"block_reward", 0, true},
		{"block_reward", -1, false},
		{"min_block_interval_ms", 0, true},
		{"min_block_interval_ms", 600_000, true},
		{"min_block_interval_ms", 600_001, false},
		{"not_a_parameter", 1, false},
	}
	for _, testCase := range cases {
		if got := GovernanceValueOK(testCase.name, testCase.value); got != testCase.ok {
			t.Errorf("GovernanceValueOK(%q, %d) = %v, want %v", testCase.name, testCase.value, got, testCase.ok)
		}
	}
}

// ---------------------------------------------------------------------- #
// Economics arithmetic
// ---------------------------------------------------------------------- #

func TestNextBaseFeeMath(t *testing.T) {
	cases := []struct {
		current, used, limit, want int64
	}{
		{InitialBaseFee, 0, 100, InitialBaseFee}, // cannot drop below the floor
		{100, 50, 100, 100},                      // exactly at target
		{100, 0, 100, 88},                        // -12.5%
		{100, 100, 100, 112},                     // +12.5%
		{10, 200, 100, 13},                       // 3 = 10*150/50/8
		{100, 0, 1, 88},                          // target floor is 1
		{100, 51, 100, 101},                      // min change is 1 when the delta rounds to zero
	}
	for _, testCase := range cases {
		if got := NextBaseFee(testCase.current, testCase.used, testCase.limit); got != testCase.want {
			t.Errorf("NextBaseFee(%d, %d, %d) = %d, want %d",
				testCase.current, testCase.used, testCase.limit, got, testCase.want)
		}
	}
}

func TestFeeRateStepsAndCap(t *testing.T) {
	cases := []struct {
		count, want int64
	}{
		{0, MinFeePerByte},
		{-5, MinFeePerByte},
		{99, MinFeePerByte},
		{100, MinFeePerByte * 2},
		{500, MinFeePerByte * 6},
		{1000, MaxFeePerByte},
		{100_000, MaxFeePerByte},
	}
	for _, testCase := range cases {
		if got := FeeRateForTransactionCount(testCase.count); got != testCase.want {
			t.Errorf("FeeRateForTransactionCount(%d) = %d, want %d", testCase.count, got, testCase.want)
		}
	}
}

// TestMerkleRootDeterministicAcrossRuns pins the tree over repeated runs and
// checks the odd-node promotion rule (no hash duplication).
func TestMerkleRootDeterministicAcrossRuns(t *testing.T) {
	leaves := []any{}
	for i := 0; i < 17; i++ {
		leaves = append(leaves, map[string]any{"i": int64(i)})
	}
	baseline := MerkleRoot(leaves)
	for run := 0; run < 100; run++ {
		if got := MerkleRoot(append([]any{}, leaves...)); got != baseline {
			t.Fatalf("merkle root changed between runs: got %s, want %s", got, baseline)
		}
	}
	if MerkleRoot(nil) != EmptyRoot {
		t.Fatalf("empty merkle root must be EmptyRoot")
	}
	proof, err := MerkleProof(leaves, 16)
	if err != nil {
		t.Fatalf("MerkleProof: %v", err)
	}
	if !VerifyMerkleProof(baseline, leaves[16], proof, 16) {
		t.Fatalf("proof for the promoted odd node did not verify")
	}
}
