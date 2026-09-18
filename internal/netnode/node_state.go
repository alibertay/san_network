package netnode

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/sanvm"
)

// ---------------------------------------------------------------------- #
// Genesis anchor and persistence
// ---------------------------------------------------------------------- #

func (n *Node) genesisAllocationFingerprint() string {
	encoded, err := canonical.Marshal(n.config.GenesisAllocations)
	if err != nil {
		return ""
	}
	return digestHex(encoded)
}

// genesisAnchorState is the state the first block of an unpruned chain starts
// from.
func (n *Node) genesisAnchorState() *anchorState {
	blockReward, _ := ledger.SanToUnits(n.config.BlockReward)
	parameters := map[string]int64{
		"min_validator_stake":   n.config.MinValidatorStake,
		"unbonding_period":      n.config.UnbondingPeriod,
		"slash_bps":             n.config.SlashBps,
		"block_gas_limit":       n.config.BlockGasLimit,
		"proposer_timeout_ms":   int64(n.config.ProposerTimeout * 1000),
		"block_reward":          blockReward,
		"min_block_interval_ms": n.config.MinBlockIntervalMs,
	}
	return &anchorState{
		Balances:     copyIntMap(n.config.GenesisAllocations),
		Nonces:       map[string]int64{},
		Validators:   map[string]map[string]any{},
		TotalSlashed: 0,
		TotalBurned:  0,
		Parameters:   parameters,
		BaseFee:      ledger.InitialBaseFee,
		Storage:      map[string]any{},
	}
}

func anchorFromSnapshot(snapshot map[string]any) *anchorState {
	state, _ := snapshot["state"].(map[string]any)
	storage, _ := snapshot["storage"].(map[string]any)
	if state == nil {
		state = map[string]any{}
	}
	if storage == nil {
		storage = map[string]any{}
	}
	return &anchorState{
		Balances:     intMapFromAny(state["balances"]),
		Nonces:       intMapFromAny(state["nonces"]),
		Validators:   validatorsFromAny(state["validators"]),
		TotalSlashed: int64Value(state["total_slashed"]),
		TotalBurned:  int64Value(state["total_burned"]),
		Parameters:   intMapFromAny(state["parameters"]),
		BaseFee:      baseFeeFromState(state),
		Storage:      storage,
	}
}

func baseFeeFromState(state map[string]any) int64 {
	if _, ok := state["base_fee"]; !ok {
		return ledger.InitialBaseFee
	}
	value := int64Value(state["base_fee"])
	if value == 0 {
		return ledger.InitialBaseFee
	}
	return value
}

func stateInputFromParts(state map[string]any, storage map[string]any) ledger.StateInput {
	if state == nil {
		state = map[string]any{}
	}
	return ledger.StateInput{
		Balances:     intMapFromAny(state["balances"]),
		Nonces:       intMapFromAny(state["nonces"]),
		Validators:   validatorsFromAny(state["validators"]),
		TotalSlashed: int64Value(state["total_slashed"]),
		Storage:      storage,
		Parameters:   intMapFromAny(state["parameters"]),
		BaseFee:      baseFeeFromState(state),
		TotalBurned:  int64Value(state["total_burned"]),
	}
}

// verifyWindow refuses a persisted chain with a broken link or hash.
func (n *Node) verifyWindow(chain []*ledger.Block) error {
	for i := 1; i < len(chain); i++ {
		previous := chain[i-1]
		current := chain[i]
		if current.Index != previous.Index+1 ||
			current.PreviousBlockHash != previous.CurrentBlockHash ||
			current.CurrentBlockHash != current.CalculateHash() {
			return fmt.Errorf("Persisted chain is corrupt at block %d; refusing to start", current.Index)
		}
	}
	return nil
}

func (n *Node) loadPersistedState(store *ledger.ChainStore) error {
	chain, err := store.LoadChain(0, nil)
	if err != nil {
		return err
	}
	if len(chain) == 0 {
		return nil
	}

	storedFinalizedHeight, hasFinalizedHeight := store.GetMeta("finalized_height")
	storedFinalizedHash, _ := store.GetMeta("finalized_hash")

	storedFingerprint, hasFingerprint := store.GetMeta("genesis_allocation")
	localFingerprint := n.genesisAllocationFingerprint()
	if hasFingerprint && storedFingerprint != localFingerprint {
		return fmt.Errorf("Persisted chain was created with a different genesis allocation (SAN_GENESIS_ALLOCATION); refusing to start")
	}

	if chain[0].Index == 0 && (len(chain) == 1 || chain[1].Index == 1) {
		expectedGenesis := n.blockchain.Tip().CurrentBlockHash
		if chain[0].CurrentBlockHash != expectedGenesis {
			return fmt.Errorf("Persisted chain genesis does not match this node's genesis (different SAN_GENESIS_ALLOCATION?); refusing to start")
		}
		if err := n.verifyWindow(chain); err != nil {
			return err
		}
		n.blockchain.Chain = chain
		state, err := store.LoadState()
		if err != nil {
			return err
		}
		if state != nil {
			n.blockchain.LoadState(state)
		}
		storage, err := store.LoadStorage()
		if err != nil {
			return err
		}
		if storage != nil {
			n.storage.LoadFromDict(storage)
		}
		n.anchorState = n.genesisAnchorState()
	} else {
		snapshots, err := store.ListSnapshots()
		if err != nil {
			return err
		}
		var snapshot map[string]any
		for _, candidate := range snapshots {
			height := int64Value(candidate["height"])
			if hash, ok := store.BlockHashAt(height); ok && hash == stringValue(candidate["hash"]) {
				snapshot = candidate
				break
			}
		}
		if snapshot == nil {
			return fmt.Errorf("Persisted chain is pruned but has no matching snapshot; refusing to start")
		}
		snapshotHeight := int64Value(snapshot["height"])
		window := []*ledger.Block{}
		for _, block := range chain {
			if block.Index >= snapshotHeight {
				window = append(window, block)
			}
		}
		if len(window) == 0 || window[0].Index != snapshotHeight {
			return fmt.Errorf("Persisted snapshot does not match the stored block window; refusing to start")
		}
		if err := n.verifyWindow(window); err != nil {
			return err
		}
		header := window[0]
		if header.StateRoot != nil {
			state, _ := snapshot["state"].(map[string]any)
			storage, _ := snapshot["storage"].(map[string]any)
			recomputed := ledger.StateRoot(stateInputFromParts(state, storage))
			if recomputed != stringValue(header.StateRoot) {
				return fmt.Errorf("Persisted snapshot state root does not match its header; refusing to start")
			}
		}
		n.anchorState = anchorFromSnapshot(snapshot)
		if !n.replayChain(window, true) {
			return fmt.Errorf("Failed to replay the stored block window; refusing to start")
		}
		log.Printf("Loaded a pruned window starting at height %d (snapshot anchor)", window[0].Index)
	}

	n.lastSeenBlockIndex = chain[len(chain)-1].Index
	n.chainHashes = map[string]struct{}{}
	for _, block := range n.blockchain.Chain {
		n.chainHashes[block.CurrentBlockHash] = struct{}{}
	}

	if err := n.requireLoadedStateRoots(chain); err != nil {
		return err
	}
	if err := n.loadFinalityState(store); err != nil {
		return err
	}
	if err := n.adoptPersistedFinality(store, storedFinalizedHeight, hasFinalizedHeight, storedFinalizedHash); err != nil {
		return err
	}

	tipBlock := n.blockAt(n.blockchain.Tip().Index)
	if tipBlock != nil && tipBlock.StateRoot != nil {
		if n.currentStateRoot() != stringValue(tipBlock.StateRoot) {
			return fmt.Errorf("Persisted state does not match the chain state root; refusing to start")
		}
	}

	dbPath := ""
	if n.config.DBPath != nil {
		dbPath = *n.config.DBPath
	}
	log.Printf("Loaded %d block(s) from %s (window starts at %d)", len(n.blockchain.Chain), dbPath, n.blockchain.Chain[0].Index)
	return nil
}

func (n *Node) requireLoadedStateRoots(chain []*ledger.Block) error {
	if !n.config.RequireStateRoot {
		return nil
	}
	for _, block := range chain {
		if block.Index > 0 && block.StateRoot == nil {
			return fmt.Errorf("Persisted block %d has no state root; refusing to start", block.Index)
		}
	}
	return nil
}

func (n *Node) loadFinalityState(store *ledger.ChainStore) error {
	raw, ok := store.GetMeta("finality_state")
	if !ok || raw == "" {
		return nil
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return fmt.Errorf("Persisted finality state is unreadable (%v); refusing to start", err)
	}
	if state == nil {
		return nil
	}
	if sets, ok := state["sets"].(map[string]any); ok {
		n.finalitySets = map[int64]map[string]int64{}
		for height, weights := range sets {
			parsed, err := strconv.ParseInt(height, 10, 64)
			if err != nil {
				continue
			}
			n.finalitySets[parsed] = intMapFromAny(weights)
		}
	}
	if votes, ok := state["votes"].(map[string]any); ok {
		n.finalityVotes = map[int64]map[string]map[string]map[string]any{}
		for height, rawVotes := range votes {
			parsed, err := strconv.ParseInt(height, 10, 64)
			if err != nil {
				continue
			}
			n.finalityVotes[parsed] = decodeFinalityVotes(rawVotes)
		}
	}
	heightValue := int64Value(state["finalized_height"])
	blockHash, _ := state["finalized_hash"].(string)
	if heightValue > 0 && blockHash != "" {
		canonicalHash, present := store.BlockHashAt(heightValue)
		if !present || canonicalHash != blockHash {
			return fmt.Errorf(
				"Persisted finality state checkpoint at height %d refers to %s but the canonical chain has %q; refusing to start",
				heightValue, blockHash, canonicalHash)
		}
		if heightValue > n.finalizedHeight {
			n.finalizedHeight = heightValue
			n.finalizedHash = blockHash
		}
	}
	return nil
}

// adoptPersistedFinality validates the checkpoint metadata against the
// canonical block index. Pruning never removes the block at the finalized
// height, so a missing or mismatching block means the persisted finality
// contradicts the chain: refuse to start instead of guessing which history is
// real (finality is monotone and can never be rolled back).
func (n *Node) adoptPersistedFinality(store *ledger.ChainStore, storedHeight string, hasHeight bool, storedHash string) error {
	if !hasHeight || storedHash == "" {
		return nil
	}
	height, err := strconv.ParseInt(storedHeight, 10, 64)
	if err != nil || height < 0 {
		return fmt.Errorf("Persisted finality checkpoint has an invalid height %q; refusing to start", storedHeight)
	}
	canonicalHash, present := store.BlockHashAt(height)
	if !present {
		return fmt.Errorf(
			"Persisted finality checkpoint at height %d is missing from the canonical chain; refusing to start", height)
	}
	if canonicalHash != storedHash {
		return fmt.Errorf(
			"Persisted finality checkpoint at height %d refers to %s but the canonical chain has %s; refusing to start",
			height, storedHash, canonicalHash)
	}
	// Finality only ever moves forward.
	if height > n.finalizedHeight {
		n.finalizedHeight = height
		n.finalizedHash = storedHash
	}
	return nil
}

func decodeFinalityVotes(raw any) map[string]map[string]map[string]any {
	result := map[string]map[string]map[string]any{}
	hashes, ok := raw.(map[string]any)
	if !ok {
		return result
	}
	for blockHash, rawVoters := range hashes {
		voters := map[string]map[string]any{}
		if records, ok := rawVoters.(map[string]any); ok {
			for address, vote := range records {
				if record, ok := vote.(map[string]any); ok {
					voters[address] = record
				}
			}
		}
		result[blockHash] = voters
	}
	return result
}

// persistFinality persists the checkpoint and recent voting bookkeeping.
func (n *Node) persistFinality() {
	if n.store == nil {
		return
	}
	keepFrom := n.blockchain.Tip().Index - 256
	sets := map[string]any{}
	for height, weights := range n.finalitySets {
		if height >= keepFrom {
			sets[strconv.FormatInt(height, 10)] = intMapToAny(weights)
		}
	}
	votes := map[string]any{}
	for height, hashes := range n.finalityVotes {
		if height < keepFrom {
			continue
		}
		encoded := map[string]any{}
		for blockHash, voters := range hashes {
			records := map[string]any{}
			for address, vote := range voters {
				records[address] = vote
			}
			encoded[blockHash] = records
		}
		votes[strconv.FormatInt(height, 10)] = encoded
	}
	state := map[string]any{
		"finalized_height": n.finalizedHeight,
		"finalized_hash":   n.finalizedHash,
		"sets":             sets,
		"votes":            votes,
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return
	}
	if err := n.store.SetMeta("finality_state", string(payload)); err != nil {
		log.Printf("Finality persistence failed: %v", err)
		return
	}
	_ = n.store.SetMeta("finalized_height", strconv.FormatInt(n.finalizedHeight, 10))
	if n.finalizedHash != "" {
		_ = n.store.SetMeta("finalized_hash", n.finalizedHash)
	}
}

func (n *Node) persistBlock(block *ledger.Block, receipts []any) {
	if n.store == nil {
		return
	}
	head := block.Index
	if err := n.store.AppendBlock(block, ledger.AppendOptions{
		Receipts: receipts,
		State:    n.blockchain.StateSnapshot(),
		Storage:  n.storage.ToDict(),
		Head:     &head,
	}); err != nil {
		log.Printf("Persistence failed for block %d: %v", block.Index, err)
		return
	}
	_ = n.store.SetMeta("genesis_allocation", n.genesisAllocationFingerprint())
}

// ---------------------------------------------------------------------- #
// Block validation and application
// ---------------------------------------------------------------------- #

// VerifyBlock validates a block: chain binding, time, continuity, hash,
// state commitment, signature, proposer round and transactions.
func (n *Node) VerifyBlock(block *ledger.Block, historical bool) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.verifyBlock(block, historical)
}

func (n *Node) verifyBlock(block *ledger.Block, historical bool) bool {
	if chainID, _ := block.ChainID.(string); chainID != n.chainID {
		log.Printf("Block %d belongs to chain %v, not %q", block.Index, block.ChainID, n.chainID)
		return false
	}

	tip := n.blockchain.Tip()
	if block.Index != tip.Index+1 {
		return false
	}
	if block.PreviousBlockHash != tip.CurrentBlockHash {
		return false
	}

	if !block.HasNumericTimestamp() {
		log.Printf("Block %d: timestamp is not numeric", block.Index)
		return false
	}
	timestamp := block.TimestampFloat()
	median := n.blockchain.MedianTimePast(11)
	if timestamp <= median {
		log.Printf("Block %d: timestamp %.0f is not after median %.0f", block.Index, timestamp, median)
		return false
	}
	now := nowSeconds()
	if timestamp > now+BlockFutureDrift {
		log.Printf("Block %d: timestamp is too far in the future", block.Index)
		return false
	}
	if !historical && timestamp < now-BlockPastDrift {
		log.Printf("Block %d: timestamp %.0f is backdated beyond the live window", block.Index, timestamp)
		return false
	}
	minInterval := n.blockchain.MinBlockInterval()
	if minInterval > 0 && timestamp < tip.TimestampFloat()+minInterval {
		log.Printf("Block %d: sooner than the minimum block interval (%.1fs)", block.Index, minInterval)
		return false
	}

	if truthy(block.RewardAddress) {
		if _, err := ledger.NormalizeAddress(block.RewardAddress); err != nil {
			log.Printf("Block %d: invalid reward address %v", block.Index, block.RewardAddress)
			return false
		}
	}

	if n.config.RequireStateRoot && block.Index > 0 && block.StateRoot == nil {
		log.Printf("Block %d: missing state root", block.Index)
		return false
	}

	if block.Round > 0 {
		required := tip.TimestampFloat() + float64(block.Round)*n.blockchain.ProposerTimeout()*0.8
		if timestamp < required {
			log.Printf("Block %d: round %d is not justified by its timestamp", block.Index, block.Round)
			return false
		}
	}

	if block.CurrentBlockHash != block.CalculateHash() {
		log.Printf("Block %d: hash mismatch", block.Index)
		return false
	}
	if !n.VerifyBlockSignature(block) {
		log.Printf("Block %d: invalid validator signature", block.Index)
		return false
	}

	active := n.blockchain.ActiveValidators()
	if len(active) > 0 {
		capRounds := int64(len(active))
		if int64(n.config.MaxProposerRounds) > capRounds {
			capRounds = int64(n.config.MaxProposerRounds)
		}
		if capRounds < 1 {
			capRounds = 1
		}
		if block.Round < 0 || block.Round >= capRounds {
			log.Printf("Block %d: implausible proposer round %d", block.Index, block.Round)
			return false
		}
		expected := n.expectedProposer(block.Index, active, block.Round)
		actual := ""
		if truthy(block.Validator) {
			if address, ok := ledger.TryAddressFromPublicKey(block.Validator); ok {
				actual = address
			}
		}
		if expected != actual {
			log.Printf("Block %d (round %d): proposer %s is not the expected %s", block.Index, block.Round, actual, expected)
			return false
		}
	}

	return n.simulateBlock(block, false) != nil
}

// simulateBlock validates a block and optionally applies its effects.
func (n *Node) simulateBlock(block *ledger.Block, apply bool) map[string]any {
	rate := n.blockchain.NextFeeRate()
	balances := copyIntMap(n.blockchain.SAN)
	nonces := copyIntMap(n.blockchain.Nonces)
	storage := sanvm.StorageFromDict(n.storage.ToDict())
	validators := copyValidators(n.blockchain.Validators)
	totalSlashed := n.blockchain.TotalSlashed
	totalBurned := n.blockchain.TotalBurned
	parameters := copyIntMap(n.blockchain.Parameters)
	baseFee := n.blockchain.BaseFee
	blockGasLimit := int64Value(parameters["block_gas_limit"])
	vm := sanvm.NewVM(storage)
	vm.Verbose = false

	totalGas := int64(0)
	totalGasUsed := int64(0)
	totalTips := int64(0)
	receipts := []any{}

	for txIndex, rawTx := range block.Transactions {
		tx, ok := rawTx.(map[string]any)
		if !ok {
			return nil
		}
		if chainID, _ := ledger.ChainIDOf(tx).(string); chainID != n.chainID {
			log.Printf("Block %d: transaction from another chain", block.Index)
			return nil
		}
		if !ledger.VerifyTransaction(tx) {
			log.Printf("Block %d: invalid transaction signature", block.Index)
			return nil
		}
		if message := ledger.ValidateGasFields(tx); message != "" {
			log.Printf("Block %d: %s", block.Index, message)
			return nil
		}
		if message := ledger.ValidateValidatorCommand(tx); message != "" {
			log.Printf("Block %d: %s", block.Index, message)
			return nil
		}
		if message := ledger.ValidateGovernanceCommand(tx); message != "" {
			log.Printf("Block %d: %s", block.Index, message)
			return nil
		}
		if int64Value(tx["fee"]) != ledger.ExpectedFee(tx, rate) {
			log.Printf("Block %d: invalid transaction fee", block.Index)
			return nil
		}

		sender, _ := tx["sender"].(string)
		if sender == "" {
			return nil
		}
		senderAddress, err := ledger.AddressFromPublicKey(sender)
		if err != nil {
			log.Printf("Block %d: invalid sender public key", block.Index)
			return nil
		}

		nonce, ok := int64Strict(tx["nonce"])
		if !ok || nonce < 0 {
			return nil
		}
		if nonce != nonces[senderAddress] {
			log.Printf("Block %d: invalid nonce for %s", block.Index, senderAddress)
			return nil
		}

		gasLimit := ledger.GasLimitOf(tx)
		totalGas += gasLimit
		if totalGas > blockGasLimit {
			log.Printf("Block %d: block gas limit exceeded", block.Index)
			return nil
		}

		gasPrice := ledger.GasPriceOf(tx)
		if ledger.HasExecution(tx) && gasPrice < baseFee {
			log.Printf("Block %d: gas price %d is below the base fee %d", block.Index, gasPrice, baseFee)
			return nil
		}

		fee := int64Value(tx["fee"])
		valueUnits := int64(0)
		if rawValue, hasValue := tx["value"]; hasValue {
			valueUnits, err = ledger.SanToUnits(rawValue)
			if err != nil {
				log.Printf("Block %d: invalid transaction value", block.Index)
				return nil
			}
			if valueUnits <= 0 {
				log.Printf("Block %d: non-positive transaction value", block.Index)
				return nil
			}
			receiver, err := ledger.NormalizeAddress(tx["receiver"])
			if err != nil {
				log.Printf("Block %d: invalid receiver address", block.Index)
				return nil
			}
			balances[receiver] = balances[receiver] + valueUnits
		}

		senderBalance := balances[senderAddress]
		if senderBalance < valueUnits+fee {
			log.Printf("Block %d: insufficient balance for %s", block.Index, senderAddress)
			return nil
		}
		balances[senderAddress] = senderBalance - valueUnits - fee
		nonces[senderAddress] = nonce + 1

		execution := map[string]any{"gas_used": int64(0), "status": "success", "error": nil, "logs": []any{}}
		if ledger.HasExecution(tx) {
			execution = n.runTxExecution(vm, storage, tx)
		}
		gasUsed := int64Value(execution["gas_used"])
		totalGasUsed += gasUsed

		refund := (gasLimit - gasUsed) * gasPrice
		balances[senderAddress] += refund
		burned := gasUsed * baseFee
		totalBurned += burned
		totalTips += fee - refund - burned

		serialized, _ := ledger.SerializeMessage(tx)
		receipts = append(receipts, map[string]any{
			"tx_index":  int64(txIndex),
			"tx_id":     digestHex(serialized),
			"sender":    senderAddress,
			"status":    execution["status"],
			"gas_used":  gasUsed,
			"gas_limit": gasLimit,
			"failure":   execution["error"],
			"logs":      execution["logs"],
		})

		if command := validatorCommandOf(tx); command != nil {
			burnedStake, ok := n.applyValidatorCommand(tx, command, senderAddress, block.Index, balances, validators)
			if !ok {
				log.Printf("Block %d: invalid validator command", block.Index)
				return nil
			}
			totalSlashed += burnedStake
		}

		if command := governanceCommandOf(tx); command != nil {
			if !n.applyGovernanceCommand(tx, command, validators, parameters) {
				log.Printf("Block %d: invalid governance command", block.Index)
				return nil
			}
		}
	}

	nextFee := ledger.NextBaseFee(baseFee, totalGasUsed, blockGasLimit)

	if truthy(block.Validator) {
		rewardAddress := ""
		if truthy(block.RewardAddress) {
			rewardAddress = stringValue(block.RewardAddress)
		} else {
			rewardAddress = n.validatorRewardAddress(stringValue(block.Validator))
		}
		subsidy := int64Value(parameters["block_reward"])
		balances[rewardAddress] = balances[rewardAddress] + totalTips + subsidy
	}

	storageSnapshot := storage.ToDict()

	if block.StateRoot != nil {
		computedRoot := ledger.StateRoot(ledger.StateInput{
			Balances:     balances,
			Nonces:       nonces,
			Validators:   validators,
			TotalSlashed: totalSlashed,
			Storage:      storageSnapshot,
			Parameters:   parameters,
			BaseFee:      nextFee,
			TotalBurned:  totalBurned,
		})
		if computedRoot != stringValue(block.StateRoot) {
			log.Printf("Block %d: state root mismatch (announced %s, computed %s)", block.Index, stringValue(block.StateRoot), computedRoot)
			return nil
		}
	}

	if apply {
		n.blockchain.SAN = balances
		n.blockchain.Nonces = nonces
		n.storage.LoadFromDict(storageSnapshot)
		n.blockchain.Validators = validators
		n.blockchain.TotalSlashed = totalSlashed
		n.blockchain.TotalBurned = totalBurned
		n.blockchain.Parameters = parameters
		n.blockchain.BaseFee = nextFee
	}

	return map[string]any{
		"balances":      balances,
		"nonces":        nonces,
		"validators":    validators,
		"total_slashed": totalSlashed,
		"total_burned":  totalBurned,
		"parameters":    parameters,
		"base_fee":      nextFee,
		"storage":       storageSnapshot,
		"receipts":      receipts,
		"gas_used":      totalGasUsed,
	}
}

func validatorCommandOf(payload map[string]any) map[string]any {
	command, ok := payload["validator"].(map[string]any)
	if !ok {
		return nil
	}
	return command
}

func governanceCommandOf(payload map[string]any) map[string]any {
	command, ok := payload["governance"].(map[string]any)
	if !ok {
		return nil
	}
	return command
}

// applyValidatorCommand applies a staking command; ok=false rejects the block.
func (n *Node) applyValidatorCommand(tx map[string]any, command map[string]any, senderAddress string,
	blockIndex int64, balances map[string]int64, validators map[string]map[string]any) (int64, bool) {
	name, _ := command["command"].(string)
	info, hasInfo := validators[senderAddress]

	switch name {
	case "deposit":
		amount := int64Value(command["amount"])
		if balances[senderAddress] < amount {
			log.Printf("Validator deposit exceeds the available balance")
			return 0, false
		}
		balances[senderAddress] -= amount
		stake := int64(0)
		joined := blockIndex
		if hasInfo {
			stake = int64Value(info["stake"])
			if value, ok := int64Strict(info["joined_height"]); ok {
				joined = value
			}
		}
		validators[senderAddress] = map[string]any{
			"public_key":     tx["sender"],
			"stake":          stake + amount,
			"joined_height":  joined,
			"release_height": nil,
		}
		return 0, true

	case "undelegate":
		if !hasInfo || info["release_height"] != nil {
			return 0, false
		}
		updated := deepCopyStringMap(info)
		updated["release_height"] = blockIndex + n.blockchain.UnbondingPeriod()
		validators[senderAddress] = updated
		return 0, true

	case "withdraw":
		if !hasInfo || info["release_height"] == nil {
			return 0, false
		}
		if blockIndex < int64Value(info["release_height"]) {
			return 0, false
		}
		balances[senderAddress] = balances[senderAddress] + int64Value(info["stake"])
		delete(validators, senderAddress)
		return 0, true

	case "evidence":
		voteA, okA := command["vote_a"].(map[string]any)
		voteB, okB := command["vote_b"].(map[string]any)
		if !okA || !okB {
			return 0, false
		}
		if !n.verifyEquivocation(voteA, voteB) {
			log.Printf("Invalid equivocation evidence")
			return 0, false
		}
		offender, ok := ledger.TryAddressFromPublicKey(voteA["public_key"])
		if !ok {
			return 0, false
		}
		offenderInfo, hasOffender := validators[offender]
		if !hasOffender {
			return 0, false
		}
		stake := int64Value(offenderInfo["stake"])
		burned := stake * n.blockchain.SlashBps() / 10_000
		remaining := stake - burned
		balances[offender] = balances[offender] + remaining
		delete(validators, offender)
		n.incMetric("slashing_events")
		log.Printf("Slashed %s: %d units burned, %d returned", offender, burned, remaining)
		return burned, true
	}
	return 0, false
}

func (n *Node) votePayload(vote map[string]any) ([]byte, error) {
	return canonical.Marshal(mapWithout(vote, []string{"signature"}))
}

func (n *Node) verifyEquivocation(voteA, voteB map[string]any) bool {
	publicKey, okKey := voteA["public_key"].(string)
	signatureA, okA := voteA["signature"].(string)
	signatureB, okB := voteB["signature"].(string)
	if !okKey || !okA || !okB {
		return false
	}
	publicKeyB, _ := voteB["public_key"].(string)
	if publicKeyB != publicKey {
		return false
	}
	for _, vote := range []map[string]any{voteA, voteB} {
		if chainID, _ := vote["chain_id"].(string); chainID != n.chainID {
			return false
		}
		if _, ok := int64Strict(vote["height"]); !ok {
			return false
		}
	}
	if int64Value(voteA["height"]) != int64Value(voteB["height"]) {
		return false
	}
	hashA, _ := voteA["block_hash"].(string)
	hashB, _ := voteB["block_hash"].(string)
	if hashA == "" || hashB == "" || hashA == hashB {
		return false
	}
	payloadA, err := n.votePayload(voteA)
	if err != nil {
		return false
	}
	payloadB, err := n.votePayload(voteB)
	if err != nil {
		return false
	}
	return ledger.VerifyIdentity(payloadA, signatureA, publicKey) &&
		ledger.VerifyIdentity(payloadB, signatureB, publicKey)
}

// applyGovernanceCommand applies a parameter change approved by 2/3 of the
// active stake.
func (n *Node) applyGovernanceCommand(tx map[string]any, command map[string]any,
	validators map[string]map[string]any, parameters map[string]int64) bool {
	name, _ := command["name"].(string)
	value, ok := int64Strict(command["value"])
	if !ok {
		log.Printf("Invalid parameter value: %v", command["value"])
		return false
	}
	if name == "" || !ledger.GovernanceValueOK(name, value) {
		log.Printf("Invalid parameter change: %s=%v", name, command["value"])
		return false
	}

	minStake := int64Value(parameters["min_validator_stake"])
	active := map[string]int64{}
	for address, info := range validators {
		if info["release_height"] != nil {
			continue
		}
		stake := int64Value(info["stake"])
		if stake >= minStake {
			active[address] = stake
		}
	}
	if len(active) == 0 {
		log.Printf("Governance rejected: no active validators")
		return false
	}

	message, err := canonical.Marshal(map[string]any{
		"chain_id":  n.chainID,
		"command":   "set_param",
		"name":      name,
		"value":     value,
		"tx_sender": tx["sender"],
		"tx_nonce":  tx["nonce"],
	})
	if err != nil {
		return false
	}

	approvedStake := int64(0)
	seen := map[string]struct{}{}
	approvals, _ := command["approvals"].([]any)
	for _, rawApproval := range approvals {
		approval, ok := rawApproval.(map[string]any)
		if !ok {
			return false
		}
		publicKey, _ := approval["public_key"].(string)
		signature, _ := approval["signature"].(string)
		if publicKey == "" || signature == "" {
			return false
		}
		address, ok := ledger.TryAddressFromPublicKey(publicKey)
		if !ok {
			return false
		}
		if _, duplicate := seen[address]; duplicate {
			return false
		}
		stake, activeValidator := active[address]
		if !activeValidator {
			return false
		}
		if !ledger.VerifyIdentity(message, signature, publicKey) {
			return false
		}
		seen[address] = struct{}{}
		approvedStake += stake
	}

	totalStake := int64(0)
	for _, stake := range active {
		totalStake += stake
	}
	if approvedStake*ledger.FinalityDenominator < totalStake*ledger.FinalityNumerator {
		log.Printf("Governance rejected: %d/%d stake approved", approvedStake, totalStake)
		return false
	}

	parameters[name] = value
	n.incMetric("governance_changes")
	log.Printf("Parameter %s set to %d by validator majority", name, value)
	return true
}

// runTxExecution executes a transaction payload and returns a receipt-like
// result. Failed executions leave no state behind but still burn the gas
// escrow, so the charged fee always matches the deterministic fee formula.
func (n *Node) runTxExecution(vm *sanvm.VM, storage *sanvm.Storage, tx map[string]any) map[string]any {
	gasLimit := ledger.GasLimitOf(tx)
	snapshot := storage.ToDict()
	fail := func(message string) map[string]any {
		storage.LoadFromDict(snapshot)
		log.Printf("Transaction execution failed (%s); gas escrow burned", message)
		return map[string]any{"gas_used": gasLimit, "status": "failed", "error": message, "logs": []any{}}
	}

	if rawBytecode, hasBytecode := tx["bytecode"]; hasBytecode {
		instructions, ok := rawBytecode.([]any)
		if !ok {
			return fail(fmt.Sprintf("Invalid bytecode: %T", rawBytecode))
		}
		bytecode, err := sanvm.ParseInstructionList(instructions)
		if err != nil {
			return fail(err.Error())
		}
		if err := vm.Run(bytecode, 0, &gasLimit); err != nil {
			return fail(err.Error())
		}
		return map[string]any{"gas_used": vm.GasUsed, "status": "success", "error": nil, "logs": logsToAny(vm.Logs)}
	}

	contractCode, _ := tx["contract_code"].(map[string]any)
	command, _ := contractCode["command"].(string)
	switch command {
	case "deploy":
		contractID, _ := contractCode["contract_id"].(string)
		var bytecode []any
		if penaCode, hasPena := contractCode["pena_code"].(string); hasPena {
			compiled, err := sanvm.CompilePena(penaCode, stringValue(contractCode["language"]))
			if err != nil {
				return fail(err.Error())
			}
			bytecode = compiled
		} else {
			raw, ok := contractCode["bytecode"].([]any)
			if !ok {
				return fail("deploy requires 'pena_code' or 'bytecode'")
			}
			bytecode = raw
		}
		if err := vm.DeployContract(contractID, bytecode, &gasLimit); err != nil {
			return fail(err.Error())
		}
		return map[string]any{
			"gas_used": vm.ContractManager.LastGasUsed,
			"status":   "success",
			"error":    nil,
			"logs":     logsToAny(vm.ContractManager.LastLogs),
		}
	case "run":
		contractID, _ := contractCode["contract_id"].(string)
		functionName, _ := contractCode["function_name"].(string)
		params, _ := contractCode["params"].([]any)
		if _, err := vm.CallContractFunction(contractID, functionName, params, &gasLimit); err != nil {
			return fail(err.Error())
		}
		return map[string]any{
			"gas_used": vm.ContractManager.LastGasUsed,
			"status":   "success",
			"error":    nil,
			"logs":     logsToAny(vm.ContractManager.LastLogs),
		}
	default:
		return fail(fmt.Sprintf("Unknown contract command: %q", command))
	}
}

func logsToAny(logs []map[string]any) []any {
	result := make([]any, len(logs))
	for i, entry := range logs {
		result[i] = entry
	}
	return result
}

// CommitBlock validates and applies a block atomically, execution included.
// The caller must hold n.mu.
func (n *Node) CommitBlock(block *ledger.Block, historical bool) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.commitBlock(block, historical)
}

func (n *Node) commitBlock(block *ledger.Block, historical bool) bool {
	if !n.verifyBlock(block, historical) {
		log.Printf("Block %d rejected by full verification", block.Index)
		return false
	}

	// Freeze the voting weights for this height before the block mutates the
	// validator registry.
	n.finalitySets[block.Index] = copyIntMap(n.blockchain.ActiveValidatorsFor(block.Index))
	if len(n.finalitySets) > 4096 {
		for height := range n.finalitySets {
			if height <= block.Index-4096 {
				delete(n.finalitySets, height)
			}
		}
	}

	outcome := n.simulateBlock(block, true)
	if outcome == nil {
		log.Printf("Block %d rejected during execution", block.Index)
		return false
	}

	receipts, _ := outcome["receipts"].([]any)
	n.receipts[block.Index] = receipts
	if len(n.receipts) > 128 {
		for index := range n.receipts {
			if index <= block.Index-128 {
				delete(n.receipts, index)
			}
		}
	}
	n.incMetric("blocks_committed")
	n.metricsMu.Lock()
	n.metrics["transactions_committed"] += int64(len(block.Transactions))
	n.metricsMu.Unlock()

	n.blockchain.Chain = append(n.blockchain.Chain, block)
	n.lastSeenBlockIndex = block.Index
	n.chainHashes[block.CurrentBlockHash] = struct{}{}
	if len(n.blockchain.ActiveValidators()) > 0 {
		n.pendingVotes[block.Index] = struct{}{}
	}
	if n.proposerState != nil && n.proposerState.Height <= block.Index {
		n.proposerState = nil
	}

	// Votes that arrived before the block itself can now be tallied; votes
	// from addresses outside the frozen set are pruned first.
	frozen := n.finalitySets[block.Index]
	if votes, ok := n.finalityVotes[block.Index]; ok {
		for pendingHash, voters := range votes {
			for address := range voters {
				if _, present := frozen[address]; !present {
					delete(voters, address)
				}
			}
			n.tallyFinality(block.Index, pendingHash)
		}
	}

	n.persistBlock(block, receipts)
	n.persistFinality()

	if n.store != nil && n.config.SnapshotInterval > 0 && block.Index > 0 &&
		block.Index%int64(n.config.SnapshotInterval) == 0 {
		if err := n.store.SaveSnapshot(block.Index, block.CurrentBlockHash,
			n.blockchain.StateSnapshot(), n.storage.ToDict()); err != nil {
			log.Printf("Snapshot failed: %v", err)
		} else {
			log.Printf("State snapshot saved at height %d", block.Index)
		}
	}

	n.prunePool()
	return true
}

// ---------------------------------------------------------------------- #
// State capture / restore / replay
// ---------------------------------------------------------------------- #

func (n *Node) captureState() *chainStateSnapshot {
	return &chainStateSnapshot{
		Chain:         append([]*ledger.Block{}, n.blockchain.Chain...),
		Balances:      copyIntMap(n.blockchain.SAN),
		Nonces:        copyIntMap(n.blockchain.Nonces),
		Validators:    copyValidators(n.blockchain.Validators),
		TotalSlashed:  n.blockchain.TotalSlashed,
		TotalBurned:   n.blockchain.TotalBurned,
		Parameters:    copyIntMap(n.blockchain.Parameters),
		BaseFee:       n.blockchain.BaseFee,
		Storage:       n.storage.ToDict(),
		ChainHashes:   copyStringSet(n.chainHashes),
		FinalitySets:  copyFinalitySets(n.finalitySets),
		FinalityVotes: copyFinalityVotes(n.finalityVotes),
		Receipts:      copyReceipts(n.receipts),
	}
}

func (n *Node) restoreState(snapshot *chainStateSnapshot) {
	n.blockchain.Chain = append([]*ledger.Block{}, snapshot.Chain...)
	n.blockchain.SAN = copyIntMap(snapshot.Balances)
	n.blockchain.Nonces = copyIntMap(snapshot.Nonces)
	n.blockchain.Validators = copyValidators(snapshot.Validators)
	n.blockchain.TotalSlashed = snapshot.TotalSlashed
	n.blockchain.TotalBurned = snapshot.TotalBurned
	n.blockchain.Parameters = copyIntMap(snapshot.Parameters)
	n.blockchain.BaseFee = snapshot.BaseFee
	n.storage.LoadFromDict(snapshot.Storage)
	n.chainHashes = copyStringSet(snapshot.ChainHashes)
	n.finalitySets = copyFinalitySets(snapshot.FinalitySets)
	n.finalityVotes = copyFinalityVotes(snapshot.FinalityVotes)
	n.receipts = copyReceipts(snapshot.Receipts)
	n.lastSeenBlockIndex = n.blockchain.Tip().Index
}

// replayChain rebuilds ledger state from a candidate chain, in memory only.
// The caller must hold n.mu (or call during construction).
func (n *Node) replayChain(chain []*ledger.Block, historical bool) bool {
	anchor := n.anchorState
	if anchor == nil {
		anchor = n.genesisAnchorState()
	}
	if len(chain) == 0 {
		return false
	}
	n.blockchain.Chain = []*ledger.Block{chain[0]}
	n.blockchain.SAN = copyIntMap(anchor.Balances)
	n.blockchain.Nonces = copyIntMap(anchor.Nonces)
	n.blockchain.Validators = copyValidators(anchor.Validators)
	n.blockchain.TotalSlashed = anchor.TotalSlashed
	n.blockchain.TotalBurned = anchor.TotalBurned
	n.blockchain.Parameters = copyIntMap(anchor.Parameters)
	n.blockchain.BaseFee = anchor.BaseFee
	n.storage.LoadFromDict(anchor.Storage)
	n.chainHashes = map[string]struct{}{chain[0].CurrentBlockHash: {}}

	for _, block := range chain[1:] {
		n.finalitySets[block.Index] = copyIntMap(n.blockchain.ActiveValidatorsFor(block.Index))
		blockHistorical := historical || block.Index <= n.finalizedHeight
		if !n.verifyBlock(block, blockHistorical) {
			return false
		}
		outcome := n.simulateBlock(block, true)
		if outcome == nil {
			return false
		}
		n.blockchain.Chain = append(n.blockchain.Chain, block)
		n.chainHashes[block.CurrentBlockHash] = struct{}{}
		receipts, _ := outcome["receipts"].([]any)
		n.receipts[block.Index] = receipts
	}

	n.lastSeenBlockIndex = n.blockchain.Tip().Index
	if n.store != nil {
		if err := n.store.ReplaceChain(n.blockchain.Chain,
			n.blockchain.StateSnapshot(), n.storage.ToDict(), n.receipts); err != nil {
			log.Printf("Chain replacement failed: %v", err)
			return false
		}
		_, _ = n.store.DeleteMismatchedSnapshots(n.canonicalHashAt)
	}
	return true
}

func (n *Node) canonicalHashAt(height int64) (string, bool) {
	block := n.blockAt(height)
	if block == nil {
		return "", false
	}
	return block.CurrentBlockHash, true
}

// validatorRewardAddress derives the reward destination from a public key.
func (n *Node) validatorRewardAddress(validator string) string {
	if derived, ok := ledger.TryAddressFromPublicKey(validator); ok {
		return derived
	}
	if validator == "UNSIGNED_VALIDATOR" {
		return validator
	}
	return validator
}

// stateRootFromOutcome recomputes the state root of a simulation outcome.
func stateRootFromOutcome(outcome map[string]any) string {
	balances, _ := outcome["balances"].(map[string]int64)
	nonces, _ := outcome["nonces"].(map[string]int64)
	validators, _ := outcome["validators"].(map[string]map[string]any)
	parameters, _ := outcome["parameters"].(map[string]int64)
	storage, _ := outcome["storage"].(map[string]any)
	return ledger.StateRoot(ledger.StateInput{
		Balances:     balances,
		Nonces:       nonces,
		Validators:   validators,
		TotalSlashed: int64Value(outcome["total_slashed"]),
		Storage:      storage,
		Parameters:   parameters,
		BaseFee:      int64Value(outcome["base_fee"]),
		TotalBurned:  int64Value(outcome["total_burned"]),
	})
}
