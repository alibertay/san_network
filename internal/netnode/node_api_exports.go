package netnode

// Small exported wrappers used by the HTTP API layer. Keeping them in a new
// file avoids touching the already-tested node implementation.

// GenesisHash returns the hash of the first block of the in-memory chain
// window (nil when the chain is empty). Mirrors app/routes.py reading
// “chain.chain[0].current_block_hash“.
func (n *Node) GenesisHash() any {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.blockchain.Chain) == 0 {
		return nil
	}
	return n.blockchain.Chain[0].CurrentBlockHash
}

// ControllerCount returns the number of selected controller nodes.
func (n *Node) ControllerCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.controllerNodes)
}

// GenesisAllocations returns the immutable genesis allocation.
func (n *Node) GenesisAllocations() map[string]int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	allocations := make(map[string]int64, len(n.blockchain.GenesisAllocations))
	for address, units := range n.blockchain.GenesisAllocations {
		allocations[address] = units
	}
	return allocations
}

// GenesisParameters returns the immutable genesis consensus parameters.
func (n *Node) GenesisParameters() map[string]int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	parameters := make(map[string]int64, len(n.blockchain.GenesisParameters))
	for name, value := range n.blockchain.GenesisParameters {
		parameters[name] = value
	}
	return parameters
}

// BaseFee returns the current base fee per gas unit.
func (n *Node) BaseFee() int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.blockchain.BaseFee
}

// ActiveValidatorCount returns the number of active validators at the tip.
func (n *Node) ActiveValidatorCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.blockchain.ActiveValidators())
}
