package netnode

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/sanlog"
)

// ---------------------------------------------------------------------- #
// Proposer rounds
// ---------------------------------------------------------------------- #

// ExpectedProposer returns the deterministic proposer for a height and round.
func (n *Node) ExpectedProposer(height int64, round int64) string {
	n.mu.Lock()
	active := n.blockchain.ActiveValidators()
	n.mu.Unlock()
	return n.expectedProposer(height, active, round)
}

func (n *Node) expectedProposer(height int64, active map[string]int64, round int64) string {
	if active == nil {
		active = n.blockchain.ActiveValidators()
	}
	if len(active) == 0 {
		return ""
	}
	ranked := make([]string, 0, len(active))
	for address := range active {
		ranked = append(ranked, address)
	}
	sort.Strings(ranked)
	seed := sha256Sum([]byte(fmt.Sprintf("%s:%d", n.chainID, height)))
	base := int64(binary.BigEndian.Uint64(seed[:8])) % int64(len(ranked))
	index := (base + round) % int64(len(ranked))
	if index < 0 {
		index += int64(len(ranked))
	}
	return ranked[index]
}

// currentRound advances the proposer round as timeouts elapse. The caller
// must hold n.mu.
func (n *Node) currentRoundLocked(height int64) int64 {
	now := timeNow()
	state := n.proposerState
	if state == nil || state.Height != height {
		n.proposerState = &proposerRoundState{Height: height, Round: 0, Started: now}
		return 0
	}
	active := n.blockchain.ActiveValidators()
	capRounds := int64(len(active))
	if int64(n.config.MaxProposerRounds) > capRounds {
		capRounds = int64(n.config.MaxProposerRounds)
	}
	if capRounds < 1 {
		capRounds = 1
	}
	timeout := n.blockchain.ProposerTimeout()
	if timeout < 0.1 {
		timeout = 0.1
	}
	elapsed := now.Sub(state.Started).Seconds()
	advance := int64(elapsed / timeout)
	if advance > capRounds-1 {
		advance = capRounds - 1
	}
	if advance > state.Round {
		state.Round = advance
		log.Printf("Height %d: proposer round advanced to %d", height, advance)
	}
	return state.Round
}

// isExpectedProposerLocked reports whether this node may propose the block.
// The caller must hold n.mu.
func (n *Node) isExpectedProposerLocked(height int64, round int64) bool {
	active := n.blockchain.ActiveValidators()
	if len(active) == 0 {
		return true // bootstrap: any node may propose
	}
	publicKey := n.GetPublicKey()
	address, ok := ledger.TryAddressFromPublicKey(publicKey)
	if !ok {
		return false
	}
	return n.expectedProposer(height, active, round) == address
}

// ---------------------------------------------------------------------- #
// Finality votes (2/3 of the active stake)
// ---------------------------------------------------------------------- #

// maybeVote votes for a newly committed block when this node is an active
// validator.
func (n *Node) maybeVote(block *ledger.Block) {
	publicKey := n.GetPublicKey()
	if publicKey == "" {
		return
	}
	address, ok := ledger.TryAddressFromPublicKey(publicKey)
	if !ok {
		return
	}
	n.mu.Lock()
	active := n.blockchain.ActiveValidators()
	_, isActive := active[address]
	n.mu.Unlock()
	if !isActive {
		log.Printf("Not voting for block %d: not an active validator", block.Index)
		return
	}

	vote := map[string]any{
		"chain_id":   n.chainID,
		"public_key": publicKey,
		"height":     block.Index,
		"block_hash": block.CurrentBlockHash,
		"timestamp":  nowSeconds(),
	}
	payload, err := n.votePayload(vote)
	if err != nil {
		return
	}
	vote["signature"] = n.identity.SignHex(payload)
	n.handleFinalityVote(vote)
}

// flushPendingVotes votes for every committed height not voted on yet.
func (n *Node) flushPendingVotes() {
	for {
		n.mu.Lock()
		if len(n.pendingVotes) == 0 {
			n.mu.Unlock()
			return
		}
		height := int64(0)
		first := true
		for candidate := range n.pendingVotes {
			if first || candidate < height {
				height = candidate
				first = false
			}
		}
		if height > n.blockchain.Tip().Index {
			n.mu.Unlock()
			return
		}
		delete(n.pendingVotes, height)
		block := n.blockAt(height)
		n.mu.Unlock()
		if block == nil {
			continue
		}
		n.maybeVote(block)
	}
}

// rebroadcastOwnVotes re-sends our own still-unfinalized votes.
func (n *Node) rebroadcastOwnVotes() {
	publicKey := n.GetPublicKey()
	address, ok := ledger.TryAddressFromPublicKey(publicKey)
	if !ok {
		return
	}

	n.mu.Lock()
	if len(n.PEERS) == 0 {
		n.mu.Unlock()
		return
	}
	windowStart := n.finalizedHeight - 8
	if tipBased := n.blockchain.Tip().Index - 64; tipBased > windowStart {
		windowStart = tipBased
	}
	if windowStart < 0 {
		windowStart = 0
	}
	type pendingVote struct {
		height int64
		vote   map[string]any
	}
	pending := []pendingVote{}
	for _, height := range sortedVoteHeights(n.finalityVotes) {
		if height <= windowStart || height > n.blockchain.Tip().Index {
			continue
		}
		for _, voters := range n.finalityVotes[height] {
			if vote, present := voters[address]; present {
				pending = append(pending, pendingVote{height: height, vote: vote})
			}
		}
	}
	peers := append([]map[string]any{}, n.PEERS...)
	n.mu.Unlock()

	for _, item := range pending {
		message, err := encodeObject(map[string]any{"type": "FINALITY_VOTE", "vote": item.vote})
		if err != nil {
			continue
		}
		for _, peer := range peers {
			n.sendToPeer(context.Background(), peer, "peer_port", string(message))
		}
	}
}

func sortedVoteHeights(input map[int64]map[string]map[string]map[string]any) []int64 {
	keys := make([]int64, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// sortedVoteHashes orders competing hashes at one height so evidence
// selection is identical on every run.
func sortedVoteHashes(input map[string]map[string]map[string]any) []string {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// broadcastVote deduplicates and gossips a finality vote.
func (n *Node) broadcastVote(vote map[string]any) {
	key := fmt.Sprintf("%v:%v:%v", vote["height"], vote["block_hash"], vote["public_key"])
	n.mu.Lock()
	if _, seen := n.seenVotes[key]; seen {
		n.mu.Unlock()
		return
	}
	n.seenVotes[key] = struct{}{}
	if len(n.seenVotes) > 8192 {
		n.seenVotes = map[string]struct{}{key: {}}
		n.incMetric("vote_seen_cache_resets")
	}
	peers := append([]map[string]any{}, n.PEERS...)
	n.mu.Unlock()

	if len(peers) == 0 {
		n.incMetric("votes_unbroadcast")
		return
	}
	message, err := encodeObject(map[string]any{"type": "FINALITY_VOTE", "vote": vote})
	if err != nil {
		n.incMetric("votes_unbroadcast")
		return
	}
	delivered := false
	for _, peer := range peers {
		if n.sendToPeer(context.Background(), peer, "peer_port", string(message)) {
			delivered = true
		}
	}
	if delivered {
		n.incMetric("votes_broadcast")
	} else {
		n.incMetric("votes_unbroadcast")
	}
}

// handleFinalityVote verifies, stores and gossips a finality vote; the block
// finalizes at 2/3 of the frozen active stake.
func (n *Node) handleFinalityVote(vote any) {
	v, ok := vote.(map[string]any)
	if !ok {
		n.incMetric("votes_dropped_invalid")
		return
	}
	publicKey, _ := v["public_key"].(string)
	signature, _ := v["signature"].(string)
	blockHash, _ := v["block_hash"].(string)

	if chainID, _ := v["chain_id"].(string); chainID != n.chainID {
		n.incMetric("votes_dropped_invalid")
		return
	}
	encoded, err := canonical.Marshal(v)
	if err != nil || len(encoded) > VoteMaxBytes {
		n.incMetric("votes_dropped_invalid")
		return
	}
	height, heightOK := int64Strict(v["height"])
	if !heightOK || height < 0 || blockHash == "" {
		n.incMetric("votes_dropped_invalid")
		return
	}
	payload, err := n.votePayload(v)
	if err != nil || !ledger.VerifyIdentity(payload, signature, publicKey) {
		n.incMetric("votes_dropped_invalid")
		return
	}

	address, hasAddress := ledger.TryAddressFromPublicKey(publicKey)
	n.incMetric("votes_seen")

	n.mu.Lock()
	weights, hasWeights := n.finalitySets[height]
	if !hasWeights {
		tipIndex := n.blockchain.Tip().Index
		if height <= tipIndex || height > tipIndex+VoteLookahead {
			n.mu.Unlock()
			n.incMetric("votes_dropped_height")
			return
		}
		if !hasAddress {
			n.mu.Unlock()
			return
		}
		heightVotes := n.finalityVotes[height]
		for _, otherHash := range sortedVoteHashes(heightVotes) {
			if otherHash == blockHash {
				continue
			}
			if previous, present := heightVotes[otherHash][address]; present {
				n.recordEvidence(address, height, previous, v)
				n.mu.Unlock()
				n.incMetric("votes_dropped_equivocation")
				return
			}
		}
		staged := heightVotes[blockHash]
		if staged == nil {
			if len(heightVotes) >= 8 || n.stagedHashCount() >= VoteLookahead*4 {
				n.mu.Unlock()
				n.incMetric("staged_votes_rejected")
				n.incMetric("votes_dropped_height")
				return
			}
			staged = map[string]map[string]any{}
			if n.finalityVotes[height] == nil {
				n.finalityVotes[height] = map[string]map[string]map[string]any{}
			}
			n.finalityVotes[height][blockHash] = staged
		}
		if _, duplicate := staged[address]; duplicate || len(staged) >= MaxStagedVotersPerHash {
			n.mu.Unlock()
			n.incMetric("votes_dropped_height")
			return
		}
		staged[address] = v
		n.mu.Unlock()
		n.broadcastVote(v)
		return
	}

	if _, isVoter := weights[address]; !hasAddress || !isVoter {
		n.mu.Unlock()
		n.incMetric("votes_dropped_voter")
		return
	}

	heightVotes := n.finalityVotes[height]
	for _, otherHash := range sortedVoteHashes(heightVotes) {
		if otherHash == blockHash {
			continue
		}
		if previous, present := heightVotes[otherHash][address]; present {
			n.recordEvidence(address, height, previous, v)
			n.mu.Unlock()
			n.incMetric("votes_dropped_equivocation")
			return
		}
	}
	staged := heightVotes[blockHash]
	if staged == nil {
		if len(heightVotes) >= 8 || n.stagedHashCount() >= VoteLookahead*4 {
			n.mu.Unlock()
			n.incMetric("votes_dropped_height")
			return
		}
		staged = map[string]map[string]any{}
		if n.finalityVotes[height] == nil {
			n.finalityVotes[height] = map[string]map[string]map[string]any{}
		}
		n.finalityVotes[height][blockHash] = staged
	}
	if _, duplicate := staged[address]; duplicate {
		n.mu.Unlock()
		n.incMetric("votes_dropped_duplicate")
		return
	}
	if len(staged) >= MaxStagedVotersPerHash {
		n.mu.Unlock()
		n.incMetric("staged_votes_rejected")
		n.incMetric("votes_dropped_height")
		return
	}
	staged[address] = v
	n.incMetric("votes_received")
	n.tallyFinality(height, blockHash)
	n.mu.Unlock()
	n.broadcastVote(v)
}

func (n *Node) stagedHashCount() int {
	total := 0
	for _, hashes := range n.finalityVotes {
		total += len(hashes)
	}
	return total
}

func (n *Node) recordEvidence(address string, height int64, voteA, voteB map[string]any) {
	evidence := map[string]any{
		"offender": address,
		"height":   height,
		"vote_a":   voteA,
		"vote_b":   voteB,
	}
	encoded, _ := canonical.Marshal(evidence)
	for _, existing := range n.equivocationEvidence {
		existingEncoded, _ := canonical.Marshal(existing)
		if string(existingEncoded) == string(encoded) {
			return
		}
	}
	n.equivocationEvidence = append(n.equivocationEvidence, evidence)
	if len(n.equivocationEvidence) > 256 {
		n.equivocationEvidence = n.equivocationEvidence[1:]
	}
	log.Printf("Equivocation detected: %s voted twice at height %d", address, height)
}

func (n *Node) tallyFinality(height int64, blockHash string) {
	if height <= n.finalizedHeight {
		return
	}
	// Weight votes with the set frozen at commit time.
	active := n.finalitySets[height]
	if len(active) == 0 {
		return
	}
	totalStake := int64(0)
	for _, stake := range active {
		totalStake += stake
	}
	votedStake := int64(0)
	for address := range n.finalityVotes[height][blockHash] {
		if stake, present := active[address]; present {
			votedStake += stake
		}
	}
	if votedStake*ledger.FinalityDenominator < totalStake*ledger.FinalityNumerator {
		return
	}

	block := n.blockAt(height)
	if block == nil || block.CurrentBlockHash != blockHash {
		return
	}

	n.finalizedHeight = height
	n.finalizedHash = blockHash
	for voteHeight := range n.finalityVotes {
		if voteHeight < height {
			delete(n.finalityVotes, voteHeight)
		}
	}
	if n.store != nil {
		n.persistFinality()
		if n.config.PruneKeep > 0 {
			cutoff := height - int64(n.config.PruneKeep)
			latest, err := n.store.LoadLatestSnapshot()
			if err != nil || latest == nil {
				// No restart anchor exists yet: pruning now would make the
				// store unloadable.
				cutoff = 0
			} else if cutoff > int64Value(latest["height"]) {
				cutoff = int64Value(latest["height"])
			}
			if cutoff > 1 {
				if pruned, err := n.store.PruneBlocksBelow(cutoff); err == nil && pruned > 0 {
					log.Printf("Pruned %d block(s) below the finalized checkpoint", pruned)
				}
			}
		}
	}
	sanlog.Info("block finalized", sanlog.Fields(
		"chain_id", n.chainID,
		"height", height,
		"block_hash", blockHash,
		"validator", block.Validator,
		"voted_stake", votedStake,
		"total_stake", totalStake,
	))
}

// AttestationState is the validator set and finality checkpoint.
func (n *Node) AttestationState() map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	active := n.blockchain.ActiveValidators()
	validators := make([]any, 0, len(active))
	totalStake := int64(0)
	for _, address := range sortedStringMapKeys(active) {
		stake := active[address]
		totalStake += stake
		validators = append(validators, map[string]any{
			"address":     address,
			"stake_units": stake,
			"stake":       ledger.UnitsToSAN(stake),
		})
	}
	return map[string]any{
		"finalized_height":  n.finalizedHeight,
		"finalized_hash":    n.finalizedHash,
		"validators":        validators,
		"total_stake_units": totalStake,
		"min_stake_units":   n.blockchain.MinValidatorStake(),
		"unbonding_period":  n.blockchain.UnbondingPeriod(),
		"slash_bps":         n.blockchain.SlashBps(),
		"base_fee":          n.blockchain.BaseFee,
		"total_burned":      n.blockchain.TotalBurned,
		"parameters":        intMapToAny(n.blockchain.Parameters),
	}
}

// PendingFinalityHeights lists the heights with in-flight votes.
func (n *Node) PendingFinalityHeights() []int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	heights := make([]int64, 0, len(n.finalityVotes))
	for height := range n.finalityVotes {
		heights = append(heights, height)
	}
	sort.Slice(heights, func(i, j int) bool { return heights[i] < heights[j] })
	if len(heights) > 10 {
		heights = heights[len(heights)-10:]
	}
	return heights
}

// EquivocationEvidence returns the collected evidence.
func (n *Node) EquivocationEvidence() []map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	evidence := make([]map[string]any, len(n.equivocationEvidence))
	copy(evidence, n.equivocationEvidence)
	return evidence
}

// ---------------------------------------------------------------------- #
// Transactions and block production
// ---------------------------------------------------------------------- #

// SubmitTransaction validates, pools and (when fees allow) commits a
// transaction.
func (n *Node) SubmitTransaction(payload map[string]any) (map[string]any, error) {
	n.mu.Lock()
	rate := n.blockchain.NextFeeRate()
	transaction, err := ledger.NewTransaction(payload, &rate)
	if err != nil {
		n.mu.Unlock()
		n.incMetric("transactions_rejected")
		return nil, fmt.Errorf("Invalid transaction: %v", err)
	}
	// Python raises the validation error straight out of submit_transaction.
	if err := n.validateTransactionForMempool(transaction); err != nil {
		n.mu.Unlock()
		n.noteTransactionRejected(err)
		return nil, err
	}
	result, committedBlock := n.admitTransaction(transaction)
	n.mu.Unlock()

	status, _ := result["status"].(string)
	switch status {
	case "committed", "pooled":
		n.incMetric("transactions_accepted")
	default:
		n.incMetric("transactions_rejected")
	}

	if committedBlock != nil {
		n.maybeVote(committedBlock)
		n.gossipBlock(committedBlock)
	}
	n.gossipTransaction(transaction)
	return result, nil
}

// noteTransactionRejected distinguishes duplicate submissions from other
// validation failures for /metrics.
func (n *Node) noteTransactionRejected(err error) {
	if strings.Contains(err.Error(), "duplicate transaction") {
		n.incMetric("transactions_duplicated")
		return
	}
	n.incMetric("transactions_rejected")
}

// IngestTransaction validates, pools and gossips a transaction; True when it
// was new.
func (n *Node) IngestTransaction(payload any) bool {
	payloadMap, ok := payload.(map[string]any)
	if !ok {
		return false
	}
	n.mu.Lock()
	rate := n.blockchain.NextFeeRate()
	transaction, err := ledger.NewTransaction(payloadMap, &rate)
	if err != nil {
		n.mu.Unlock()
		n.incMetric("transactions_rejected")
		log.Printf("Rejected transaction: %v", err)
		return false
	}
	if err := n.validateTransactionForMempool(transaction); err != nil {
		n.mu.Unlock()
		n.noteTransactionRejected(err)
		log.Printf("Rejected transaction: %v", err)
		return false
	}
	txID := ledger.TxID(transaction.Payload)
	if _, pooled := n.poolTxIDs[txID]; pooled {
		n.mu.Unlock()
		n.incMetric("transactions_duplicated")
		return false
	}
	if _, seen := n.seenTxGossip[txID]; seen {
		n.mu.Unlock()
		n.incMetric("transactions_duplicated")
		return false
	}
	n.transactionPool = append(n.transactionPool, transaction)
	n.poolTxIDs[txID] = struct{}{}
	n.mu.Unlock()

	n.incMetric("transactions_accepted")
	sanlog.Info("transaction accepted", sanlog.Fields("tx_id", txID))
	n.maybeProduceFromPool()
	n.gossipTransaction(transaction)
	return true
}

// admitTransaction is mempool admission and block production. The caller
// holds n.mu on entry and exit; the returned block (non-nil only when the
// transaction committed) still needs a vote and gossip.
func (n *Node) admitTransaction(transaction *ledger.Transaction) (map[string]any, *ledger.Block) {
	if err := n.validateTransactionForMempool(transaction); err != nil {
		return map[string]any{"status": "rejected", "reason": err.Error()}, nil
	}
	txID := ledger.TxID(transaction.Payload)

	n.transactionPool = append(n.transactionPool, transaction)
	n.poolTxIDs[txID] = struct{}{}

	n.prunePool()
	totalFee := int64(0)
	for _, tx := range n.transactionPool {
		totalFee += tx.Fee
	}
	thresholdUnits, _ := ledger.SanToUnits(n.config.BlockThresholdFee)
	if totalFee < thresholdUnits {
		return map[string]any{
			"status":              "pooled",
			"tx_id":               txID,
			"fee":                 transaction.Fee,
			"pooled_transactions": int64(len(n.transactionPool)),
			"total_fee":           totalFee,
			"threshold":           thresholdUnits,
		}, nil
	}

	if len(n.transactionPool) == 0 {
		return map[string]any{"status": "pooled", "tx_id": txID, "fee": transaction.Fee}, nil
	}

	if !n.isCurrentProposerLocked() {
		return map[string]any{
			"status":              "pooled",
			"tx_id":               txID,
			"fee":                 transaction.Fee,
			"pooled_transactions": int64(len(n.transactionPool)),
			"note":                "forwarded to the current proposer",
		}, nil
	}

	block := n.buildBlock(n.currentRoundLocked(n.blockchain.Tip().Index + 1))
	controllers := append([]map[string]any{}, n.controllerNodes...)
	n.mu.Unlock()
	quorum := n.sendToControllers(context.Background(), controllers, block)
	n.mu.Lock()
	if !quorum {
		return map[string]any{
			"status":              "pooled",
			"tx_id":               txID,
			"fee":                 transaction.Fee,
			"pooled_transactions": int64(len(n.transactionPool)),
			"note":                "controller quorum not reached; retrying",
		}, nil
	}

	if !n.commitBlock(block, false) {
		kept := []*ledger.Transaction{}
		for _, tx := range n.transactionPool {
			if ledger.TxID(tx.Payload) != txID {
				kept = append(kept, tx)
			}
		}
		n.transactionPool = kept
		delete(n.poolTxIDs, txID)
		return map[string]any{
			"status": "rejected",
			"tx_id":  txID,
			"fee":    transaction.Fee,
			"reason": "block failed state validation",
		}, nil
	}

	n.transactionPool = []*ledger.Transaction{}
	n.poolTxIDs = map[string]struct{}{}
	return map[string]any{
		"status":      "committed",
		"tx_id":       txID,
		"fee":         transaction.Fee,
		"block_index": block.Index,
		"block_hash":  block.CurrentBlockHash,
	}, block
}

func (n *Node) isCurrentProposerLocked() bool {
	height := n.blockchain.Tip().Index + 1
	return n.isExpectedProposerLocked(height, n.currentRoundLocked(height))
}

func (n *Node) validateTransactionForMempool(transaction *ledger.Transaction) error {
	payload := transaction.Payload

	// Bound the pool before admitting anything new; prune first so stale
	// entries cannot wedge admission forever. (The Python reference has no
	// cap; the Go node adds one as a DoS guard.)
	if n.config.MaxMempool > 0 && len(n.transactionPool) >= n.config.MaxMempool {
		n.prunePool()
		if len(n.transactionPool) >= n.config.MaxMempool {
			n.incMetric("mempool_rejected")
			return fmt.Errorf("mempool is full (%d transactions)", len(n.transactionPool))
		}
	}

	if chainID, _ := ledger.ChainIDOf(payload).(string); chainID != n.chainID {
		return fmt.Errorf("transaction chain_id does not match this chain (%s)", n.chainID)
	}
	if !ledger.VerifyTransaction(payload) {
		return fmt.Errorf("invalid transaction signature")
	}
	sender, _ := payload["sender"].(string)
	if sender == "" {
		return fmt.Errorf("transaction is missing 'sender'")
	}
	senderAddress, ok := ledger.TryAddressFromPublicKey(sender)
	if !ok {
		return fmt.Errorf("transaction 'sender' is not a valid public key")
	}
	nonce, ok := int64Strict(payload["nonce"])
	if !ok || nonce < 0 {
		return fmt.Errorf("transaction is missing a valid integer 'nonce'")
	}
	txID := ledger.TxID(payload)
	if _, duplicate := n.poolTxIDs[txID]; duplicate {
		return fmt.Errorf("duplicate transaction")
	}

	expectedNonce := n.blockchain.Nonces[senderAddress] + n.pendingCountFor(senderAddress)
	if nonce != expectedNonce {
		return fmt.Errorf("invalid nonce: expected %d, got %d", expectedNonce, nonce)
	}

	valueUnits, err := txValueUnits(transaction)
	if err != nil {
		return err
	}
	if _, hasValue := payload["value"]; hasValue {
		if valueUnits <= 0 {
			return fmt.Errorf("'value' must be a positive amount")
		}
		if _, err := ledger.NormalizeAddress(payload["receiver"]); err != nil {
			return fmt.Errorf("invalid 'receiver' address: %v", err)
		}
	}

	available := n.blockchain.SAN[senderAddress] - n.pendingSpendFor(senderAddress)
	if available < valueUnits+transaction.Fee {
		return fmt.Errorf("insufficient balance")
	}

	gasLimit := ledger.GasLimitOf(payload)
	if gasLimit > n.blockchain.BlockGasLimit() {
		return fmt.Errorf("gas_limit %d exceeds the block gas limit %d", gasLimit, n.blockchain.BlockGasLimit())
	}
	return nil
}

func (n *Node) pendingCountFor(senderAddress string) int64 {
	count := int64(0)
	for _, tx := range n.transactionPool {
		if address, ok := txSenderAddress(tx); ok && address == senderAddress {
			count++
		}
	}
	return count
}

func (n *Node) pendingSpendFor(senderAddress string) int64 {
	spent := int64(0)
	for _, tx := range n.transactionPool {
		address, ok := txSenderAddress(tx)
		if !ok || address != senderAddress {
			continue
		}
		if value, err := txValueUnits(tx); err == nil {
			spent += value
		}
		spent += tx.Fee
	}
	return spent
}

// prunePool drops transactions that no longer fit the current chain state.
func (n *Node) prunePool() {
	rate := n.blockchain.NextFeeRate()
	nextNonce := map[string]int64{}
	kept := []*ledger.Transaction{}

	for _, tx := range n.transactionPool {
		senderAddress, ok := txSenderAddress(tx)
		if !ok {
			continue
		}
		expected, present := nextNonce[senderAddress]
		if !present {
			expected = n.blockchain.Nonces[senderAddress]
		}
		nonce, _ := int64Strict(tx.Payload["nonce"])
		if nonce != expected {
			continue
		}
		if tx.Fee != ledger.ExpectedFee(tx.Payload, rate) {
			continue
		}
		nextNonce[senderAddress] = expected + 1
		kept = append(kept, tx)
	}

	n.transactionPool = kept
	poolIDs := map[string]struct{}{}
	for _, tx := range kept {
		poolIDs[ledger.TxID(tx.Payload)] = struct{}{}
	}
	n.poolTxIDs = poolIDs
}

// buildBlock assembles a block from the mempool and predicts its state root.
// The caller must hold n.mu.
func (n *Node) buildBlock(round int64) *ledger.Block {
	tip := n.blockchain.Tip()
	minimumTimestamp := tip.TimestampFloat() + n.blockchain.MinBlockInterval()
	if round > 0 {
		required := tip.TimestampFloat() + float64(round)*n.blockchain.ProposerTimeout()*0.8
		if required > minimumTimestamp {
			minimumTimestamp = required
		}
	}

	transactions := []any{}
	totalGas := int64(0)
	blockGasLimit := n.blockchain.BlockGasLimit()
	for _, tx := range n.transactionPool {
		gasLimit := ledger.GasLimitOf(tx.Payload)
		if totalGas+gasLimit > blockGasLimit {
			continue
		}
		transactions = append(transactions, tx.Payload)
		totalGas += gasLimit
	}

	blockTimestamp := nowSeconds()
	if minimumTimestamp > blockTimestamp {
		blockTimestamp = minimumTimestamp
	}

	publicKey := n.GetPublicKey()
	validator := publicKey
	if validator == "" {
		validator = "UNSIGNED_VALIDATOR"
	}
	rewardAddress := n.rewardAddress
	if rewardAddress == "" {
		rewardAddress = n.validatorRewardAddress(validator)
	}

	provisional := ledger.NewBlock(tip.Index+1, tip.CurrentBlockHash, validator, nil,
		transactions, blockTimestamp, n.chainID, nil, round, rewardAddress)
	predictedRoot := ""
	if outcome := n.simulateBlock(provisional, false); outcome != nil {
		predictedRoot = stateRootFromOutcome(outcome)
	}

	block := ledger.NewBlock(tip.Index+1, tip.CurrentBlockHash, validator, nil,
		transactions, blockTimestamp, n.chainID, predictedRoot, round, rewardAddress)
	block.ValidatorSignature = n.SignBlockHash(block.CurrentBlockHash)
	return block
}

// maybeProduceFromPool produces a block when this node is the expected
// proposer and fees allow it.
func (n *Node) maybeProduceFromPool() {
	n.mu.Lock()
	if len(n.transactionPool) == 0 {
		// Fee-only chains produce blocks on demand; a chain with a block
		// subsidy keeps producing (empty) blocks.
		if n.blockchain.BlockReward() <= 0 {
			n.mu.Unlock()
			return
		}
		tipAge := nowSeconds() - n.blockchain.Tip().TimestampFloat()
		if tipAge < n.blockchain.ProposerTimeout() {
			n.mu.Unlock()
			return
		}
	}
	if n.config.RequireBlockSig && !n.identity.CanSign() {
		n.mu.Unlock()
		log.Printf("Cannot produce blocks: no signing key configured")
		return
	}

	height := n.blockchain.Tip().Index + 1
	round := n.currentRoundLocked(height)
	if !n.isExpectedProposerLocked(height, round) {
		n.mu.Unlock()
		return
	}

	if len(n.transactionPool) > 0 {
		totalFee := int64(0)
		for _, tx := range n.transactionPool {
			totalFee += tx.Fee
		}
		thresholdUnits, _ := ledger.SanToUnits(n.config.BlockThresholdFee)
		if totalFee < thresholdUnits {
			n.mu.Unlock()
			return
		}
	}

	block := n.buildBlock(round)
	controllers := append([]map[string]any{}, n.controllerNodes...)
	n.mu.Unlock()

	if !n.sendToControllers(context.Background(), controllers, block) {
		return
	}

	n.mu.Lock()
	if !n.commitBlock(block, false) {
		n.mu.Unlock()
		return
	}
	n.transactionPool = []*ledger.Transaction{}
	n.poolTxIDs = map[string]struct{}{}
	n.mu.Unlock()

	n.maybeVote(block)
	n.gossipBlock(block)
}

// RequestMempool asks a peer for its mempool.
func (n *Node) RequestMempool(ctx context.Context, peer map[string]any) int {
	if peer == nil {
		n.mu.Lock()
		if n.outgoingNode != nil {
			peer = n.outgoingNode
		} else if len(n.PEERS) > 0 {
			peer = n.PEERS[0]
		}
		n.mu.Unlock()
	}
	if peer == nil {
		return 0
	}
	stream, err := OpenSession(ctx, n, peer, "peer_port", n.sessionTimeout())
	if err != nil {
		log.Printf("Mempool request failed: %v", err)
		return 0
	}
	defer stream.Close()
	message, _ := encodeObject(map[string]any{"type": "GET_TXS"})
	if err := stream.Send(ctx, string(message)); err != nil {
		return 0
	}
	raw, err := stream.Recv(ctx)
	if err != nil {
		return 0
	}
	data, err := decodeObject(raw)
	if err != nil {
		return 0
	}
	if messageType, _ := data["type"].(string); messageType != "TXS" {
		return 0
	}
	accepted := 0
	txs, _ := data["txs"].([]any)
	if len(txs) > 64 {
		txs = txs[:64]
	}
	for _, payload := range txs {
		if n.IngestTransaction(payload) {
			accepted++
		}
	}
	if accepted > 0 {
		log.Printf("Pulled %d transaction(s) from %s", accepted, PeerLabel(peer))
	}
	return accepted
}

// GossipTransaction announces a transaction to every peer.
func (n *Node) gossipTransaction(transaction *ledger.Transaction) {
	txID := ledger.TxID(transaction.Payload)
	n.mu.Lock()
	if _, seen := n.seenTxGossip[txID]; seen {
		n.mu.Unlock()
		return
	}
	n.seenTxGossip[txID] = struct{}{}
	if len(n.seenTxGossip) > 8192 {
		n.seenTxGossip = map[string]struct{}{txID: {}}
	}
	peers := append([]map[string]any{}, n.PEERS...)
	n.mu.Unlock()

	if len(peers) == 0 {
		return
	}
	message, err := encodeObject(map[string]any{"type": "TX", "tx": transaction.Payload})
	if err != nil {
		return
	}
	for _, peer := range peers {
		n.sendToPeer(context.Background(), peer, "peer_port", string(message))
	}
}

// ---------------------------------------------------------------------- #
// Transaction accessors
// ---------------------------------------------------------------------- #

func txSenderAddress(tx *ledger.Transaction) (string, bool) {
	sender, _ := tx.Payload["sender"].(string)
	if sender == "" {
		return "", false
	}
	return ledger.TryAddressFromPublicKey(sender)
}

func txValueUnits(tx *ledger.Transaction) (int64, error) {
	if _, present := tx.Payload["value"]; !present {
		return 0, nil
	}
	return ledger.SanToUnits(tx.Payload["value"])
}
