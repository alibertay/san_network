package netnode

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/netproto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ---------------------------------------------------------------------- #
// Synchronization
// ---------------------------------------------------------------------- #

// GetSyncPayload is the server side of Sync: one page of blocks plus a state
// snapshot on the first page.
func (n *Node) GetSyncPayload(fromIndex int64, limit int) map[string]any {
	page := limit
	if page < 1 {
		page = 1
	}
	if page > 512 {
		page = 512
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	pageLimit := int64(page + 1)
	blocks := n.blockchain.BlocksSince(fromIndex, &pageLimit)
	payload := map[string]any{
		"version":            int64(ledger.SchemaVersion),
		"chain_id":           n.chainID,
		"genesis_allocation": n.genesisAllocationFingerprint(),
		"blocks":             []any{},
		"storage":            nil,
		"state":              nil,
		"has_more":           false,
	}
	if len(blocks) == 0 {
		return payload
	}

	batch := blocks
	if len(batch) > page {
		batch = batch[:page]
	}
	blockDicts := make([]any, 0, len(batch))
	for _, block := range batch {
		blockDicts = append(blockDicts, block.ToDict())
	}
	payload["blocks"] = blockDicts
	hasMore := len(blocks) > page
	payload["has_more"] = hasMore
	if hasMore {
		payload["next_from_index"] = batch[len(batch)-1].Index + 1
	}
	if fromIndex <= 0 {
		// Only the first page carries the snapshot; later pages are just
		// blocks (the client derives state by replaying them).
		payload["storage"] = n.storage.ToDict()
		payload["state"] = n.blockchain.StateSnapshot()
	}
	return payload
}

// selectSyncPeer picks the compatible peer with the longest chain.
func (n *Node) selectSyncPeer(ctx context.Context) map[string]any {
	n.mu.Lock()
	peers := append([]map[string]any{}, n.PEERS...)
	if len(peers) > 8 {
		peers = peers[:8]
	}
	incoming := n.incomingNode
	n.mu.Unlock()
	if len(peers) == 0 {
		return nil
	}

	bestHeight := int64(-1)
	var best map[string]any
	for _, peer := range peers {
		status := RemoteStatus(ctx, n, peer, 5*time.Second)
		if status == nil {
			continue
		}
		if statusChainID, _ := status["chain_id"].(string); statusChainID != n.chainID {
			continue
		}
		if peerAllocation, _ := status["genesis_allocation"].(string); peerAllocation != "" &&
			peerAllocation != n.genesisAllocationFingerprint() {
			continue
		}
		height := int64Value(status["height"])
		if best == nil || height > bestHeight {
			best = peer
			bestHeight = height
		}
	}
	if best == nil {
		if incoming != nil {
			return incoming
		}
		return peers[0]
	}
	log.Printf("Longest compatible peer: %s at height %d (local %d)", PeerLabel(best), bestHeight, n.blockchain.Tip().Index)
	return best
}

// Synchronize pulls missing blocks from a peer and verifies them one by one.
// Ledger state is derived by replaying verified blocks, never adopted from a
// peer's snapshot.
func (n *Node) Synchronize(ctx context.Context) bool {
	n.mu.Lock()
	hasPeers := len(n.PEERS) > 0
	n.mu.Unlock()
	if !hasPeers {
		return false
	}

	peer := n.selectSyncPeer(ctx)
	if peer == nil {
		return false
	}
	batchLimit := n.config.SyncBatchSize
	if batchLimit < 1 {
		batchLimit = 1
	}
	maxBlocks := n.config.SyncMaxBlocks
	if maxBlocks < batchLimit {
		maxBlocks = batchLimit
	}

	applied := 0
	n.mu.Lock()
	fromIndex := n.blockchain.Tip().Index + 1
	n.mu.Unlock()

	for applied < maxBlocks {
		payload, err := n.remoteSync(ctx, peer, fromIndex, batchLimit)
		if err != nil {
			log.Printf("Sync with %s failed: %v", PeerLabel(peer), err)
			return applied > 0
		}

		if peerAllocation, _ := payload["genesis_allocation"].(string); peerAllocation != "" &&
			peerAllocation != n.genesisAllocationFingerprint() {
			log.Printf("Sync: peer %s uses a different genesis allocation; refusing to sync", PeerLabel(peer))
			return false
		}
		if peerChain, _ := payload["chain_id"].(string); peerChain != "" && peerChain != n.chainID {
			log.Printf("Sync: peer %s is on chain %q while this node is on %q", PeerLabel(peer), peerChain, n.chainID)
			return false
		}
		version := int64(ledger.SchemaVersion)
		if rawVersion, present := payload["version"]; present {
			parsed, ok := int64Strict(rawVersion)
			if !ok {
				log.Printf("Sync: invalid schema version %v from %s", rawVersion, PeerLabel(peer))
				return applied > 0
			}
			version = parsed
		}
		if version > ledger.SchemaVersion {
			log.Printf("Sync: peer %s uses schema %d but this node supports %d",
				PeerLabel(peer), version, ledger.SchemaVersion)
			return applied > 0
		}

		batch, _ := payload["blocks"].([]any)
		if len(batch) == 0 {
			break
		}

		stop := false
		n.mu.Lock()
		for _, rawBlock := range batch {
			blockData, ok := rawBlock.(map[string]any)
			if !ok {
				stop = true
				break
			}
			block, err := ledger.BlockFromDict(blockData)
			if err != nil {
				log.Printf("Sync: invalid block from %s: %v", PeerLabel(peer), err)
				stop = true
				break
			}

			tipFresh := n.blockchain.Tip().TimestampFloat() > nowSeconds()-BlockPastDrift
			historical := !(tipFresh && block.Index == n.blockchain.Tip().Index+1)
			if !n.verifyBlock(block, historical) {
				log.Printf("Sync: block %d from %s failed verification; stopping", block.Index, PeerLabel(peer))
				stop = true
				break
			}
			if !n.commitBlock(block, historical) {
				stop = true
				break
			}
			applied++
			fromIndex = block.Index + 1
		}
		n.mu.Unlock()

		if stop {
			break
		}
		if hasMore, _ := payload["has_more"].(bool); !hasMore {
			break
		}
	}

	if applied > 0 {
		n.mu.Lock()
		n.connectOrphans()
		n.lastSeenBlockIndex = n.blockchain.Tip().Index
		n.mu.Unlock()
		n.flushPendingVotes()
		log.Printf("Synced %d block(s) from %s", applied, PeerLabel(peer))
		// A peer with a longer chain is the best source of fresh peers.
		n.requestPeers(ctx)
	}
	return applied > 0
}

// ---------------------------------------------------------------------- #
// Block requests and controller votes
// ---------------------------------------------------------------------- #

func (n *Node) findBlockByHash(blockHash string) *ledger.Block {
	n.mu.Lock()
	defer n.mu.Unlock()
	if block, ok := n.orphans[blockHash]; ok {
		return block
	}
	if n.store != nil {
		block, err := n.store.BlockByHash(blockHash)
		if err == nil && block != nil {
			return block
		}
	}
	if _, ok := n.chainHashes[blockHash]; ok {
		for _, block := range n.blockchain.Chain {
			if block.CurrentBlockHash == blockHash {
				return block
			}
		}
	}
	return nil
}

// ServeBlockRequest answers GET_BLOCK with the block body when we have it.
func (n *Node) serveBlockRequest(ctx context.Context, stream *PeerStream, blockHash any) {
	hash, ok := blockHash.(string)
	if !ok || hash == "" {
		return
	}
	block := n.findBlockByHash(hash)
	if block == nil {
		message, _ := encodeObject(map[string]any{"type": "BLOCK_NOT_FOUND", "block_hash": hash})
		_ = stream.Send(ctx, string(message))
		return
	}
	message, _ := encodeObject(map[string]any{"type": "BLOCK", "block": block.ToDict()})
	_ = stream.Send(ctx, string(message))
}

// requestBlock fetches one missing block (e.g. an orphan's parent) from a peer.
// Peers that recently answered BLOCK_NOT_FOUND for the hash are tried last; if
// every candidate lacks the block, a chain sync is attempted because the block
// may live on a longer branch we have not seen yet.
func (n *Node) requestBlock(ctx context.Context, blockHash string) bool {
	if blockHash == "" {
		return false
	}
	n.mu.Lock()
	if _, requested := n.requestedBlocks[blockHash]; requested {
		n.mu.Unlock()
		return false
	}
	if len(n.requestedBlocks) > 512 {
		n.requestedBlocks = map[string]struct{}{}
	}
	n.requestedBlocks[blockHash] = struct{}{}
	n.mu.Unlock()

	peers := n.blockFetchPeers(blockHash)
	fetched := false
	notFound := 0
	for _, peer := range peers {
		accepted, missing := n.fetchBlock(ctx, peer, blockHash)
		if accepted {
			fetched = true
			break
		}
		if missing {
			notFound++
			n.markPeerMissingBlock(peer, blockHash)
		}
	}
	if fetched {
		n.clearBlockMisses(blockHash)
		return true
	}
	// Allow a later gossip message to retry with a different peer.
	n.mu.Lock()
	delete(n.requestedBlocks, blockHash)
	n.mu.Unlock()
	if len(peers) > 0 && notFound == len(peers) {
		// All candidates lack the block: it may live on a longer branch we
		// have not seen. Throttle the catch-up attempt to one per peer-check
		// interval so a batch of missing parents cannot start a sync storm.
		n.mu.Lock()
		now := nowSeconds()
		allowSync := now-n.blockMissSyncAt >= n.config.PeerCheckInterval
		if allowSync {
			n.blockMissSyncAt = now
		}
		n.mu.Unlock()
		if allowSync {
			log.Printf("No peer has block %.12s; trying a chain sync", blockHash)
			n.Synchronize(ctx)
		}
	}
	return false
}

// blockFetchPeers orders the block-request candidates: the outgoing peer
// first, then every known peer, with peers that recently reported the hash as
// BLOCK_NOT_FOUND moved to the back. When everyone is marked they are all
// still tried, so a peer that later learns the block is not starved.
func (n *Node) blockFetchPeers(blockHash string) []map[string]any {
	n.mu.Lock()
	peers := append([]map[string]any{}, n.PEERS...)
	outgoing := n.outgoingNode
	n.mu.Unlock()

	ordered := make([]map[string]any, 0, len(peers)+1)
	seen := map[string]struct{}{}
	add := func(peer map[string]any) {
		if peer == nil {
			return
		}
		label := PeerLabel(peer)
		if _, duplicate := seen[label]; duplicate {
			return
		}
		seen[label] = struct{}{}
		ordered = append(ordered, peer)
	}
	add(outgoing)
	for _, peer := range peers {
		add(peer)
	}

	preferred := make([]map[string]any, 0, len(ordered))
	deferred := make([]map[string]any, 0, len(ordered))
	for _, peer := range ordered {
		if n.peerMissingBlock(peer, blockHash) {
			deferred = append(deferred, peer)
		} else {
			preferred = append(preferred, peer)
		}
	}
	return append(preferred, deferred...)
}

// markPeerMissingBlock remembers that a peer answered BLOCK_NOT_FOUND for a
// hash. Marks expire after BlockMissTTL and the map is bounded.
func (n *Node) markPeerMissingBlock(peer map[string]any, blockHash string) {
	if peer == nil || blockHash == "" {
		return
	}
	label := PeerLabel(peer)
	now := nowSeconds()
	n.mu.Lock()
	defer n.mu.Unlock()
	for hash, misses := range n.peerMissingBlocks {
		for known, seen := range misses {
			if now-seen > BlockMissTTL {
				delete(misses, known)
			}
		}
		if len(misses) == 0 {
			delete(n.peerMissingBlocks, hash)
		}
	}
	misses, ok := n.peerMissingBlocks[blockHash]
	if !ok {
		if len(n.peerMissingBlocks) >= 512 {
			n.peerMissingBlocks = map[string]map[string]float64{}
		}
		misses = map[string]float64{}
		n.peerMissingBlocks[blockHash] = misses
	}
	misses[label] = now
}

// peerMissingBlock reports whether a peer recently answered BLOCK_NOT_FOUND
// for a hash.
func (n *Node) peerMissingBlock(peer map[string]any, blockHash string) bool {
	label := PeerLabel(peer)
	n.mu.Lock()
	defer n.mu.Unlock()
	misses, ok := n.peerMissingBlocks[blockHash]
	if !ok {
		return false
	}
	seen, present := misses[label]
	return present && nowSeconds()-seen <= BlockMissTTL
}

// clearBlockMisses forgets the BLOCK_NOT_FOUND marks for a hash once the block
// has been delivered or otherwise seen.
func (n *Node) clearBlockMisses(blockHash string) {
	n.mu.Lock()
	delete(n.peerMissingBlocks, blockHash)
	n.mu.Unlock()
}

// classifyBlockReply parses a GET_BLOCK reply: (block, notFound, ok).
func classifyBlockReply(data map[string]any) (*ledger.Block, bool, bool) {
	switch messageType, _ := data["type"].(string); messageType {
	case "BLOCK_NOT_FOUND":
		return nil, true, true
	case "BLOCK":
		blockData, ok := data["block"].(map[string]any)
		if !ok {
			return nil, false, false
		}
		block, err := ledger.BlockFromDict(blockData)
		if err != nil {
			return nil, false, false
		}
		return block, false, true
	default:
		return nil, false, false
	}
}

// fetchBlock requests one block body from one peer and tries to connect it.
// It returns (accepted, notFound); notFound is true only for an explicit
// BLOCK_NOT_FOUND reply so the caller can deprioritise that peer.
func (n *Node) fetchBlock(ctx context.Context, peer map[string]any, blockHash string) (bool, bool) {
	stream, err := OpenSession(ctx, n, peer, "p2p_port", n.sessionTimeout())
	if err != nil {
		log.Printf("Block request for %.12s failed: %v", blockHash, err)
		return false, false
	}
	defer stream.Close()
	message, _ := encodeObject(map[string]any{"type": "GET_BLOCK", "block_hash": blockHash})
	if err := stream.Send(ctx, string(message)); err != nil {
		return false, false
	}
	raw, err := stream.Recv(ctx)
	if err != nil {
		return false, false
	}
	data, err := decodeObject(raw)
	if err != nil {
		return false, false
	}
	block, notFound, ok := classifyBlockReply(data)
	if !ok {
		return false, false
	}
	if notFound {
		return false, true
	}

	n.mu.Lock()
	accepted := n.processIncomingBlock(block)
	n.mu.Unlock()
	if accepted {
		n.flushPendingVotes()
	}
	return accepted, false
}

// sendToControllers requests approvals from the controller set and gossips the
// block when a 2/3 majority approves.
func (n *Node) sendToControllers(ctx context.Context, controllers []map[string]any, block *ledger.Block) bool {
	if len(controllers) == 0 {
		log.Printf("No controller nodes; accepting block %d locally", block.Index)
		return true
	}
	approvals := 0
	for _, controller := range controllers {
		if n.requestBlockVote(ctx, controller, block) {
			approvals++
		}
	}
	ratio := float64(approvals) / float64(len(controllers))
	log.Printf("Block %d controller approval: %d/%d (%.2f)", block.Index, approvals, len(controllers), ratio)
	if ratio < 0.66 {
		return false
	}
	n.gossipBlock(block)
	return true
}

func (n *Node) requestBlockVote(ctx context.Context, controller map[string]any, block *ledger.Block) bool {
	message, err := encodeObject(map[string]any{"type": "BLOCK_VOTE_REQUEST", "block": block.ToDict()})
	if err != nil {
		return false
	}
	stream, err := OpenSession(ctx, n, controller, "controller_port", n.sessionTimeout())
	if err != nil {
		log.Printf("Controller %s did not vote: %v", PeerLabel(controller), err)
		return false
	}
	defer stream.Close()

	if err := stream.Send(ctx, string(message)); err != nil {
		return false
	}
	raw, err := stream.Recv(ctx)
	if err != nil {
		log.Printf("Controller %s did not vote: %v", PeerLabel(controller), err)
		return false
	}
	data, err := decodeObject(raw)
	if err != nil {
		return false
	}

	if messageType, _ := data["type"].(string); messageType != "BLOCK_VOTE_RESPONSE" {
		return false
	}
	if approved, _ := data["approved"].(bool); !approved {
		return false
	}
	if chainID, _ := data["chain_id"].(string); chainID != n.chainID {
		log.Printf("Controller %s voted for another chain", PeerLabel(controller))
		return false
	}
	if blockHash, _ := data["block_hash"].(string); blockHash != block.CurrentBlockHash {
		log.Printf("Controller %s voted for a different block", PeerLabel(controller))
		return false
	}
	signature, _ := data["signature"].(string)
	publicKey, _ := data["public_key"].(string)
	if signature == "" || publicKey == "" {
		log.Printf("Controller %s sent an unsigned vote", PeerLabel(controller))
		return false
	}
	if expected, _ := controller["public_key"].(string); expected != "" && expected != publicKey {
		log.Printf("Controller %s vote key does not match its advertised key", PeerLabel(controller))
		return false
	}
	payload := mapWithout(data, voteMetaFields)
	signedBytes, err := canonical.Marshal(payload)
	if err != nil {
		return false
	}
	return ledger.VerifyIdentity(signedBytes, signature, publicKey)
}

// serveBlockVote answers a controller quorum vote request over the session.
func (n *Node) serveBlockVote(ctx context.Context, stream *PeerStream, data map[string]any) {
	approved := false
	blockHash := ""
	if blockData, ok := data["block"].(map[string]any); ok {
		if block, err := ledger.BlockFromDict(blockData); err == nil {
			blockHash = block.CurrentBlockHash
			approved = n.VerifyBlock(block, false)
		} else {
			log.Printf("Block vote failed: %v", err)
		}
	}
	response := map[string]any{
		"type":       "BLOCK_VOTE_RESPONSE",
		"chain_id":   n.chainID,
		"approved":   approved,
		"block_hash": blockHash,
	}
	if signature := n.SignVote(response); signature != "" {
		response["signature"] = signature
		response["public_key"] = n.GetPublicKey()
	}
	message, _ := encodeObject(response)
	_ = stream.Send(ctx, string(message))
}

// handleIncomingBlock processes a BLOCK message.
func (n *Node) handleIncomingBlock(ctx context.Context, data map[string]any) {
	blockData, ok := data["block"].(map[string]any)
	if !ok {
		log.Printf("Received malformed block")
		return
	}
	block, err := ledger.BlockFromDict(blockData)
	if err != nil {
		log.Printf("Received malformed block: %v", err)
		return
	}
	n.clearBlockMisses(block.CurrentBlockHash)

	n.mu.Lock()
	accepted := n.processIncomingBlock(block)
	tip := n.blockchain.Tip()
	shouldSync := false
	if accepted {
		n.syncMisses = 0
	} else if block.Index == tip.Index+1 && block.PreviousBlockHash == tip.CurrentBlockHash {
		// A tip-adjacent block we could not verify: catch up instead of
		// stalling.
		n.syncMisses++
		if n.syncMisses >= 2 {
			n.syncMisses = 0
			shouldSync = true
		}
	}
	n.mu.Unlock()

	if shouldSync {
		// One-shot gossip sessions are canceled as soon as the sender closes
		// them, so follow-up networking must not inherit this context.
		n.Synchronize(context.Background())
	}
	if accepted && n.config.BlockGossip {
		n.gossipBlock(block)
	}
	if accepted {
		n.maybeVote(block)
		n.flushPendingVotes()
	}
	if accepted {
		n.mu.Lock()
		_, parentOnChain := n.chainHashes[block.PreviousBlockHash]
		_, parentOrphan := n.orphans[block.PreviousBlockHash]
		n.mu.Unlock()
		if !parentOnChain && !parentOrphan {
			n.requestBlock(context.Background(), block.PreviousBlockHash)
		}
	}
}

// GossipBlock gossips a block once; deduplication keeps the network loop-free.
func (n *Node) GossipBlock(block *ledger.Block) {
	n.gossipBlock(block)
}

func (n *Node) gossipBlock(block *ledger.Block) {
	blockHash := block.CurrentBlockHash
	n.mu.Lock()
	if _, seen := n.seenBlockGossip[blockHash]; seen {
		n.mu.Unlock()
		return
	}
	n.seenBlockGossip[blockHash] = struct{}{}
	if len(n.seenBlockGossip) > 4096 {
		n.seenBlockGossip = map[string]struct{}{blockHash: {}}
	}
	peers := append([]map[string]any{}, n.PEERS...)
	n.mu.Unlock()

	if len(peers) == 0 {
		log.Printf("No peers; block %d not broadcast", block.Index)
		return
	}
	message, err := encodeObject(map[string]any{"type": "BLOCK", "block": block.ToDict()})
	if err != nil {
		return
	}
	for _, peer := range peers {
		n.sendToPeer(context.Background(), peer, "p2p_port", string(message))
	}
}

// BroadcastBlock broadcasts a locally produced block to every known peer.
func (n *Node) BroadcastBlock(block *ledger.Block) {
	n.gossipBlock(block)
}

// ---------------------------------------------------------------------- #
// Canonical Sync RPC client
// ---------------------------------------------------------------------- #

// dialPeer mirrors transport.dial_peer: bounded message sizes, optional TLS
// with the peer host pinned as authority.
func (n *Node) dialPeer(peer map[string]any, portKey string) (*grpc.ClientConn, error) {
	options := []grpc.DialOption{
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(n.WSMaxSize()),
			grpc.MaxCallSendMsgSize(n.WSMaxSize()),
		),
	}
	if truthy(peer["tls"]) {
		options = append(options, grpc.WithAuthority(stringValue(peer["host"])))
		creds := n.TransportClientCredentials(peer)
		if creds == nil {
			return nil, fmt.Errorf("TLS requested but no client credentials")
		}
		options = append(options, grpc.WithTransportCredentials(creds))
	} else {
		options = append(options, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	return grpc.NewClient(stringValue(peer["host"])+":"+stringValue(peer[portKey]), options...)
}

// remoteSync fetches one page of blocks, decoding with Python json.loads
// semantics so int/float values keep their exact representation.
func (n *Node) remoteSync(ctx context.Context, peer map[string]any, fromIndex int64, limit int) (map[string]any, error) {
	connection, err := n.dialPeer(peer, "p2p_port")
	if err != nil {
		return nil, err
	}
	defer connection.Close()

	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	response, err := netproto.NewP2PClient(connection).Sync(callCtx,
		&netproto.SyncRequest{FromIndex: fromIndex, Limit: int32(limit)})
	if err != nil {
		return nil, err
	}
	decoded, err := canonical.Decode(response.Payload)
	if err != nil {
		return nil, err
	}
	payload, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("sync payload is not an object")
	}
	return payload, nil
}
