package netnode

import (
	"log"
	"sort"

	"github.com/alibertay/san_network/internal/ledger"
)

// ---------------------------------------------------------------------- #
// Fork handling (longest valid chain wins)
// ---------------------------------------------------------------------- #

// verifyForkBlock applies the cheap checks for a block whose branch state is
// not known yet.
func (n *Node) verifyForkBlock(block *ledger.Block) bool {
	if block.CurrentBlockHash != block.CalculateHash() {
		return false
	}
	if !n.VerifyBlockSignature(block) {
		return false
	}
	for _, rawTx := range block.Transactions {
		tx, ok := rawTx.(map[string]any)
		if !ok || !ledger.VerifyTransaction(tx) {
			return false
		}
	}
	return true
}

// sortedOrphanHashes returns the buffered orphan hashes in lexicographic
// order so every map-driven fork decision is reproducible across runs.
func (n *Node) sortedOrphanHashes() []string {
	hashes := make([]string, 0, len(n.orphans))
	for blockHash := range n.orphans {
		hashes = append(hashes, blockHash)
	}
	sort.Strings(hashes)
	return hashes
}

func (n *Node) bufferOrphan(block *ledger.Block) {
	n.orphans[block.CurrentBlockHash] = block
	for len(n.orphans) > n.config.MaxOrphans {
		var oldest *ledger.Block
		for _, candidate := range n.orphans {
			if oldest == nil || candidate.Index < oldest.Index ||
				(candidate.Index == oldest.Index && candidate.CurrentBlockHash < oldest.CurrentBlockHash) {
				oldest = candidate
			}
		}
		if oldest == nil {
			break
		}
		delete(n.orphans, oldest.CurrentBlockHash)
		n.incMetric("orphans_evicted")
		if oldest.CurrentBlockHash == block.CurrentBlockHash {
			break
		}
	}
}

// connectOrphans extends the chain with buffered blocks whose parent is the
// tip.
func (n *Node) connectOrphans() int {
	connected := 0
	progressed := true
	for progressed {
		progressed = false
		for _, blockHash := range n.sortedOrphanHashes() {
			block, present := n.orphans[blockHash]
			if !present {
				continue
			}
			if block.PreviousBlockHash != n.blockchain.Tip().CurrentBlockHash {
				continue
			}
			delete(n.orphans, blockHash)
			if !n.verifyBlock(block, false) || !n.commitBlock(block, false) {
				log.Printf("Orphan block %d failed to connect", block.Index)
				continue
			}
			connected++
			progressed = true
		}
	}
	return connected
}

// processIncomingBlock accepts an extension, or buffers a competing branch for
// reorg. The caller must hold n.mu.
func (n *Node) processIncomingBlock(block *ledger.Block) bool {
	if _, known := n.chainHashes[block.CurrentBlockHash]; known {
		return false
	}

	tip := n.blockchain.Tip()
	if block.PreviousBlockHash == tip.CurrentBlockHash {
		if !n.verifyBlock(block, false) {
			log.Printf("Block %d failed verification; ignored", block.Index)
			return false
		}
		n.commitBlock(block, false)
		n.connectOrphans()
		return true
	}

	if _, knownParent := n.chainHashes[block.PreviousBlockHash]; knownParent {
		return n.bufferForkBlock(block)
	}
	if _, orphanParent := n.orphans[block.PreviousBlockHash]; orphanParent {
		return n.bufferForkBlock(block)
	}

	// Unknown parent: keep it for a bounded window so out-of-order gossip can
	// still assemble a branch once the missing ancestor arrives.
	if block.Index > tip.Index+int64(n.config.MaxOrphans) {
		return false
	}
	if block.Index <= tip.Index-int64(n.config.MaxReorgDepth) {
		return false
	}
	if !n.verifyForkBlock(block) {
		log.Printf("Orphan block %d failed signature checks", block.Index)
		return false
	}
	n.bufferOrphan(block)
	n.connectOrphans()
	if len(n.orphans) > 0 {
		n.tryReorg()
	}
	return true
}

func (n *Node) bufferForkBlock(block *ledger.Block) bool {
	tip := n.blockchain.Tip()
	if block.Index <= tip.Index-int64(n.config.MaxReorgDepth) {
		log.Printf("Ignoring fork block %d beyond the reorg depth", block.Index)
		return false
	}
	if !n.verifyForkBlock(block) {
		log.Printf("Fork block %d failed signature checks", block.Index)
		return false
	}
	n.bufferOrphan(block)
	n.connectOrphans()
	if len(n.orphans) > 0 {
		n.tryReorg()
	}
	return true
}

// bestOrphanChain builds the longest candidate chain that connects to a known
// ancestor.
func (n *Node) bestOrphanChain() []*ledger.Block {
	if len(n.orphans) == 0 {
		return nil
	}
	chain := n.blockchain.Chain
	chainIndex := map[string]int{}
	for index, block := range chain {
		chainIndex[block.CurrentBlockHash] = index
	}
	var best []*ledger.Block

	for _, orphanHash := range n.sortedOrphanHashes() {
		orphan := n.orphans[orphanHash]
		branch := []*ledger.Block{orphan}
		cursor := orphan
		seen := map[string]struct{}{orphan.CurrentBlockHash: {}}
		for {
			parentHash := cursor.PreviousBlockHash
			if forkIndex, ok := chainIndex[parentHash]; ok {
				candidate := make([]*ledger.Block, 0, forkIndex+1+len(branch))
				candidate = append(candidate, chain[:forkIndex+1]...)
				for i := len(branch) - 1; i >= 0; i-- {
					candidate = append(candidate, branch[i])
				}
				if best == nil || len(candidate) > len(best) ||
					(len(candidate) == len(best) &&
						candidate[len(candidate)-1].CurrentBlockHash < best[len(best)-1].CurrentBlockHash) {
					best = candidate
				}
				break
			}
			parentBlock, ok := n.orphans[parentHash]
			if !ok {
				break
			}
			if _, loop := seen[parentBlock.CurrentBlockHash]; loop {
				break
			}
			seen[parentBlock.CurrentBlockHash] = struct{}{}
			branch = append(branch, parentBlock)
			cursor = parentBlock
		}
	}
	return best
}

// requeueTransactions puts transactions from reorged-out blocks back into the
// mempool. The caller must hold n.mu.
func (n *Node) requeueTransactions(oldChain, newChain []*ledger.Block) {
	newHashes := map[string]struct{}{}
	for _, block := range newChain {
		newHashes[block.CurrentBlockHash] = struct{}{}
	}
	rate := n.blockchain.NextFeeRate()

	for _, block := range oldChain {
		if _, present := newHashes[block.CurrentBlockHash]; present {
			continue
		}
		for _, rawTx := range block.Transactions {
			txDict, ok := rawTx.(map[string]any)
			if !ok {
				continue
			}
			transaction, err := ledger.NewTransaction(txDict, &rate)
			if err != nil {
				continue
			}
			if err := n.validateTransactionForMempool(transaction); err != nil {
				continue
			}
			txID := ledger.TxID(transaction.Payload)
			if _, pooled := n.poolTxIDs[txID]; pooled {
				continue
			}
			n.transactionPool = append(n.transactionPool, transaction)
			n.poolTxIDs[txID] = struct{}{}
		}
	}
	n.prunePool()
}

// tryReorg switches to the longest valid branch. The caller must hold n.mu.
func (n *Node) tryReorg() bool {
	candidate := n.bestOrphanChain()
	if candidate == nil || len(candidate) <= len(n.blockchain.Chain) {
		return false
	}

	// Finalized history can never be rewritten. The candidate may start at a
	// pruned snapshot height, so the finalized checkpoint is addressed by its
	// absolute block height, never by a slice index.
	if n.finalizedHeight > 0 {
		offset := n.finalizedHeight - candidate[0].Index
		if offset < 0 || offset >= int64(len(candidate)) {
			log.Printf("Rejected reorg shorter than finalized history")
			return false
		}
		if candidate[offset].CurrentBlockHash != n.finalizedHash {
			log.Printf("Rejected reorg that would rewrite finalized history")
			return false
		}
	}

	oldChain := append([]*ledger.Block{}, n.blockchain.Chain...)
	snapshot := n.captureState()

	if !n.replayChain(candidate, false) {
		log.Printf("Reorg candidate failed during replay; restoring old chain")
		n.restoreState(snapshot)
		return false
	}

	n.incMetric("reorgs")
	n.requeueTransactions(oldChain, candidate)
	if n.store != nil {
		_, _ = n.store.DeleteMismatchedSnapshots(n.canonicalHashAt)
	}
	n.persistFinality()
	for _, blockHash := range n.sortedOrphanHashes() {
		if _, onChain := n.chainHashes[blockHash]; onChain {
			delete(n.orphans, blockHash)
		}
	}
	// Retain the replaced branch's blocks as orphans (they were fully verified
	// before they became canonical) so a later, longer branch descending from
	// them can be assembled instead of being unreachable until re-fetched.
	// The orphan buffer stays bounded by max_orphans.
	for _, block := range oldChain {
		if _, onChain := n.chainHashes[block.CurrentBlockHash]; onChain {
			continue
		}
		n.bufferOrphan(block)
	}
	log.Printf("Reorg: switched to a %d-block chain (tip index %d, hash %s)",
		len(candidate), n.blockchain.Tip().Index, n.blockchain.Tip().CurrentBlockHash)
	return true
}
