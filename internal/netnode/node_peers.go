package netnode

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/ledger"
)

// ---------------------------------------------------------------------- #
// Peer records (signed, time bounded)
// ---------------------------------------------------------------------- #

func (n *Node) peerRecordPayload(record map[string]any) ([]byte, error) {
	return canonical.Marshal(mapWithout(record, peerMetaFields))
}

// SelfPeerRecord builds the signed record this node announces.
func (n *Node) SelfPeerRecord() map[string]any {
	host := ""
	if n.config.AdvertiseHost != nil {
		host = *n.config.AdvertiseHost
	}
	if host == "" {
		host = n.GetLocalIP()
	}
	record := map[string]any{
		"chain_id":        n.chainID,
		"host":            host,
		"api_port":        int64(n.config.APIPort),
		"p2p_port":        int64(n.config.P2PPort),
		"peer_port":       int64(n.config.PeerPort),
		"controller_port": int64(n.config.ControllerPort),
		"timestamp":       nowSeconds(),
		"tls":             n.config.TLSEnabled(),
	}
	if publicKey := n.identity.PublicKeyHex(); publicKey != "" {
		record["public_key"] = publicKey
		if payload, err := n.peerRecordPayload(record); err == nil {
			if signature := n.identity.SignHex(payload); signature != "" {
				record["signature"] = signature
			}
		}
	}
	return record
}

// VerifyPeerRecord validates the chain binding, freshness and signature.
func (n *Node) VerifyPeerRecord(record map[string]any) bool {
	if chainID, _ := record["chain_id"].(string); chainID != n.chainID {
		log.Printf("Rejected peer record for chain %v", record["chain_id"])
		return false
	}
	publicKey, _ := record["public_key"].(string)
	signature, _ := record["signature"].(string)
	if publicKey == "" || signature == "" {
		if n.config.RequireBlockSig {
			log.Printf("Rejected unsigned peer record from %v", record["host"])
			return false
		}
		return true
	}
	if timestamp, present := record["timestamp"]; present && timestamp != nil {
		value, ok := numericValue(timestamp)
		if !ok {
			return false
		}
		age := nowSeconds() - value
		if age > n.config.PeerRecordTTL || age < -n.config.PeerRecordTTL {
			log.Printf("Rejected stale peer record from %v (age %.0fs)", record["host"], age)
			return false
		}
	}
	payload, err := n.peerRecordPayload(record)
	if err != nil {
		return false
	}
	return ledger.VerifyIdentity(payload, signature, publicKey)
}

// seenRecently deduplicates peer updates by identity (ports + key), ignoring
// the timestamp so periodic re-announcements do not trigger re-gossip.
func (n *Node) seenRecently(record map[string]any) bool {
	identity := mapWithout(record, []string{"signature", "timestamp"})
	encoded, err := canonical.Marshal(identity)
	if err != nil {
		return false
	}
	digest := digestHex(encoded)
	now := nowSeconds()
	cutoff := now - n.config.PeerRecordTTL

	n.mu.Lock()
	defer n.mu.Unlock()
	for key, seen := range n.seenPeerUpdates {
		if seen < cutoff {
			delete(n.seenPeerUpdates, key)
		}
	}
	if _, present := n.seenPeerUpdates[digest]; present {
		return true
	}
	n.seenPeerUpdates[digest] = now
	return false
}

// RegisterToNetwork announces this node to the outgoing peer.
func (n *Node) RegisterToNetwork(ctx context.Context) bool {
	n.mu.Lock()
	outgoing := n.outgoingNode
	n.mu.Unlock()
	if outgoing == nil {
		log.Printf("No outgoing node configured; cannot register")
		return false
	}
	message, err := encodeObject(map[string]any{"type": "PEER_UPDATE", "peer": n.SelfPeerRecord()})
	if err != nil {
		return false
	}
	return n.sendToPeer(ctx, outgoing, "peer_port", string(message))
}

func (n *Node) isSelf(peer map[string]any) bool {
	localHosts := map[string]bool{
		"127.0.0.1":    true,
		"localhost":    true,
		"0.0.0.0":      true,
		n.GetLocalIP(): true,
	}
	host, _ := peer["host"].(string)
	return localHosts[host] && int64Value(peer["api_port"]) == int64(n.config.APIPort)
}

func (n *Node) mergePeers(peers any) int {
	return n.mergePeersFrom(peers, AddrSourceGossip)
}

// mergePeersFrom merges records and records their discovery source in the
// address manager.
func (n *Node) mergePeersFrom(peers any, source string) int {
	added := 0
	switch typed := peers.(type) {
	case []any:
		for _, raw := range typed {
			added += n.addPeerFrom(raw, source)
		}
	case []map[string]any:
		for _, raw := range typed {
			added += n.addPeerFrom(raw, source)
		}
	}
	return added
}

// addRecords merges registry/cache records and refreshes the selection.
func (n *Node) addRecords(records []map[string]any, source string) int {
	added := n.mergePeersFrom(records, source)
	if added > 0 {
		n.refreshPeerSelection()
	}
	return added
}

func (n *Node) addPeer(raw any) int {
	return n.addPeerFrom(raw, AddrSourceGossip)
}

func (n *Node) addPeerFrom(raw any, source string) int {
	record, ok := CompletePeer(raw, n.config)
	if !ok || n.isSelf(record) {
		return 0
	}
	if !n.VerifyPeerRecord(record) {
		return 0
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	for _, known := range n.PEERS {
		if SamePeer(record, known) {
			return 0
		}
	}
	if len(n.PEERS) >= n.config.MaxPeers {
		log.Printf("Peer limit reached (%d); ignoring %s", n.config.MaxPeers, PeerLabel(record))
		return 0
	}
	n.PEERS = append(n.PEERS, record)
	if n.addrman != nil {
		n.addrman.Add(record, source)
	}
	return 1
}

func (n *Node) removePeer(peer map[string]any) {
	if peer == nil {
		return
	}
	n.mu.Lock()
	kept := []map[string]any{}
	for _, known := range n.PEERS {
		if !SamePeer(known, peer) {
			kept = append(kept, known)
		}
	}
	n.PEERS = kept
	n.mu.Unlock()
	n.refreshPeerSelection()
}

// AddPeers merges discovered peers and refreshes the selection.
func (n *Node) AddPeers(peers any) int {
	added := n.mergePeers(peers)
	n.refreshPeerSelection()
	return added
}

// AddPeer merges a single peer record (returns 1 when added).
func (n *Node) AddPeer(raw any) int {
	added := n.addPeer(raw)
	if added > 0 {
		n.refreshPeerSelection()
	}
	return added
}

// ---------------------------------------------------------------------- #
// Controller selection (deterministic per epoch)
// ---------------------------------------------------------------------- #

func (n *Node) controllerEligible(peer map[string]any) bool {
	publicKey, _ := peer["public_key"].(string)
	if publicKey == "" {
		return false
	}
	if n.config.ControllerMinStake > 0 {
		address, ok := ledger.TryAddressFromPublicKey(publicKey)
		if !ok {
			return false
		}
		n.mu.Lock()
		balance := n.blockchain.SAN[address]
		n.mu.Unlock()
		return balance >= n.config.ControllerMinStake
	}
	return true
}

func (n *Node) selectControllers() []map[string]any {
	n.mu.Lock()
	peers := append([]map[string]any{}, n.PEERS...)
	epochLength := n.config.EpochLength
	if epochLength < 1 {
		epochLength = 1
	}
	epoch := n.blockchain.Tip().Index / int64(epochLength)
	n.mu.Unlock()

	eligible := []map[string]any{}
	for _, peer := range peers {
		if n.controllerEligible(peer) {
			eligible = append(eligible, peer)
		}
	}
	if len(eligible) == 0 {
		return []map[string]any{}
	}

	type scoredPeer struct {
		score []byte
		peer  map[string]any
	}
	scored := make([]scoredPeer, 0, len(eligible))
	for _, peer := range eligible {
		publicKey, _ := peer["public_key"].(string)
		score := sha256Sum([]byte(joinEpochScore(epoch, publicKey)))
		scored = append(scored, scoredPeer{score: score[:], peer: peer})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		return bytes.Compare(scored[i].score, scored[j].score) < 0
	})
	count := n.config.ControllerCount
	if count > len(scored) {
		count = len(scored)
	}
	result := make([]map[string]any, 0, count)
	for _, item := range scored[:count] {
		result = append(result, item.peer)
	}
	return result
}

func joinEpochScore(epoch int64, publicKey string) string {
	return fmt.Sprintf("%d:%s", epoch, publicKey)
}

func (n *Node) refreshPeerSelection() {
	n.mu.Lock()
	pool := append([]map[string]any{}, n.PEERS...)
	n.mu.Unlock()
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })

	var incoming, outgoing map[string]any
	if len(pool) > 0 {
		incoming = pool[0]
		outgoing = pool[0]
		if len(pool) > 1 {
			outgoing = pool[1]
		}
	}
	controllers := n.selectControllers()
	n.mu.Lock()
	n.incomingNode = incoming
	n.outgoingNode = outgoing
	n.controllerNodes = controllers
	n.mu.Unlock()
}

// ---------------------------------------------------------------------- #
// Background loops and health checks
// ---------------------------------------------------------------------- #

func (n *Node) blockProductionLoop(ctx context.Context) {
	for {
		n.mu.Lock()
		interval := n.blockchain.ProposerTimeout()
		n.mu.Unlock()
		if interval > 2.0 {
			interval = 2.0
		}
		if interval < 0.5 {
			interval = 0.5
		}
		timer := time.NewTimer(time.Duration(interval * float64(time.Second)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					log.Printf("Block production loop failed: %v", recovered)
				}
			}()
			n.maybeProduceFromPool()
		}()
	}
}

func (n *Node) peerHealthLoop(ctx context.Context) {
	for {
		timer := time.NewTimer(time.Duration(n.config.PeerCheckInterval * float64(time.Second)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					log.Printf("Peer health check failed: %v", recovered)
				}
			}()
			n.CheckDeadPeers(ctx)
			n.mu.Lock()
			hasPeers := len(n.PEERS) > 0
			n.mu.Unlock()
			if hasPeers {
				n.RegisterToNetwork(ctx)
				// Pull the peer list so discovery converges even when the
				// REST /bootstrap endpoint is not reachable.
				n.requestPeers(ctx)
			}
			n.maybeProduceFromPool()
			n.flushPendingVotes()
			n.rebroadcastOwnVotes()

			n.mu.Lock()
			missing := []string{}
			for _, block := range n.orphans {
				_, onChain := n.chainHashes[block.PreviousBlockHash]
				_, orphaned := n.orphans[block.PreviousBlockHash]
				if !onChain && !orphaned {
					missing = append(missing, block.PreviousBlockHash)
				}
			}
			height := n.blockchain.Tip().Index + 1
			expected := n.isExpectedProposerLocked(height, n.currentRoundLocked(height))
			emptyPool := len(n.transactionPool) == 0
			n.mu.Unlock()

			if len(missing) > 8 {
				missing = missing[:8]
			}
			for _, parentHash := range missing {
				n.requestBlock(ctx, parentHash)
			}
			if hasPeers && (emptyPool || expected) {
				n.RequestMempool(ctx, nil)
			}
		}()
	}
}

// CheckDeadPeers pings every known peer and drops the ones that keep not
// answering.
func (n *Node) CheckDeadPeers(ctx context.Context) {
	n.mu.Lock()
	peers := append([]map[string]any{}, n.PEERS...)
	n.mu.Unlock()

	for _, peer := range peers {
		key := stringValue(peer["host"]) + ":" + stringValue(peer["api_port"])
		if n.PingNode(ctx, peer) {
			n.mu.Lock()
			delete(n.peerFailures, key)
			n.mu.Unlock()
			continue
		}
		n.mu.Lock()
		failures := n.peerFailures[key] + 1
		n.peerFailures[key] = failures
		n.mu.Unlock()
		if failures < n.config.PeerMissThreshold {
			log.Printf("Peer %s missed a health check (%d/%d)", PeerLabel(peer), failures, n.config.PeerMissThreshold)
			continue
		}
		log.Printf("Peer %s is unresponsive; removing", PeerLabel(peer))
		n.mu.Lock()
		delete(n.peerFailures, key)
		n.mu.Unlock()
		n.removePeer(peer)
		n.gossipDeadPeer(ctx, peer)
	}
	n.refreshPeerSelection()
}

// PingNode returns true only when the peer completes a handshake and answers
// PING.
func (n *Node) PingNode(ctx context.Context, peer map[string]any) bool {
	stream, err := OpenSession(ctx, n, peer, "peer_port", n.sessionTimeout())
	if err != nil {
		log.Printf("Ping to %s failed: %v", PeerLabel(peer), err)
		return false
	}
	defer stream.Close()
	if err := stream.Send(ctx, `{"type":"PING"}`); err != nil {
		return false
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := stream.Recv(callCtx)
	if err != nil {
		return false
	}
	data, err := decodeObject(raw)
	if err != nil {
		return false
	}
	messageType, _ := data["type"].(string)
	return messageType == "PONG"
}

// GossipPeers announces a newly learned peer to every known peer.
func (n *Node) gossipPeers(ctx context.Context, newPeer map[string]any) bool {
	n.mu.Lock()
	peers := append([]map[string]any{}, n.PEERS...)
	n.mu.Unlock()
	if len(peers) == 0 {
		return false
	}
	message, err := encodeObject(map[string]any{"type": "PEER_UPDATE", "peer": newPeer})
	if err != nil {
		return false
	}
	delivered := false
	for _, peer := range peers {
		if SamePeer(peer, newPeer) {
			continue
		}
		if n.sendToPeer(ctx, peer, "peer_port", string(message)) {
			delivered = true
		}
	}
	return delivered
}

func (n *Node) gossipDeadPeer(ctx context.Context, deadPeer map[string]any) bool {
	n.mu.Lock()
	outgoing := n.outgoingNode
	n.mu.Unlock()
	if outgoing == nil {
		return false
	}
	message, err := encodeObject(map[string]any{"type": "DEAD_PEER", "peer": deadPeer})
	if err != nil {
		return false
	}
	return n.sendToPeer(ctx, outgoing, "peer_port", string(message))
}

// sendToPeer opens a one-shot gRPC session: handshake, one message, close.
func (n *Node) sendToPeer(ctx context.Context, peer map[string]any, portKey, message string) bool {
	if peer == nil {
		return false
	}
	stream, err := OpenSession(ctx, n, peer, portKey, n.sessionTimeout())
	if err != nil {
		log.Printf("Could not send message to %s (%s): %v", PeerLabel(peer), portKey, err)
		return false
	}
	defer stream.Close()
	if err := stream.Send(ctx, message); err != nil {
		log.Printf("Could not send message to %s (%s): %v", PeerLabel(peer), portKey, err)
		return false
	}
	return true
}

// ---------------------------------------------------------------------- #
// Bootstrap
// ---------------------------------------------------------------------- #

func (n *Node) bootstrap(ctx context.Context) {
	addresses := n.config.BootstrapAddresses()
	if len(addresses) == 0 {
		return
	}
	for _, address := range addresses {
		var peers []any
		remotePeers, err := RemoteBootstrap(ctx, n, address, 5*time.Second)
		if err != nil {
			log.Printf("gRPC bootstrap %s failed (%v); trying the REST endpoint", address, err)
		} else {
			peers = remotePeers
		}
		if len(peers) == 0 {
			peers = n.DiscoverPeers(address)
		}
		n.mergePeersFrom(peers, AddrSourceBootstrap)
	}
	n.refreshPeerSelection()
	n.mu.Lock()
	hasPeers := len(n.PEERS) > 0
	n.mu.Unlock()
	if !hasPeers {
		log.Printf("Bootstrap %s returned no usable peers", strings.Join(addresses, ", "))
		return
	}
	n.RegisterToNetwork(ctx)
	// The seed list may be partial; ask a peer directly over the session
	// stream for its current view.
	n.requestPeers(ctx)
}

// ---------------------------------------------------------------------- #
// Peer list exchange (GET_PEERS / PEERS)
// ---------------------------------------------------------------------- #

// requestPeers asks a selected peer for its peer list over the session stream
// (GET_PEERS -> PEERS) and merges the reply. It returns the number of newly
// added records. Python has no client path for this message; the Go node uses
// it from bootstrap, the health loop and after a successful sync so peer
// discovery converges without the REST endpoint.
func (n *Node) requestPeers(ctx context.Context) int {
	n.mu.Lock()
	peer := n.outgoingNode
	if peer == nil && len(n.PEERS) > 0 {
		peer = n.PEERS[0]
	}
	n.mu.Unlock()
	if peer == nil {
		return 0
	}

	stream, err := OpenSession(ctx, n, peer, "peer_port", n.sessionTimeout())
	if err != nil {
		log.Printf("Peer list request to %s failed: %v", PeerLabel(peer), err)
		return 0
	}
	defer stream.Close()

	message, err := encodeObject(map[string]any{"type": "GET_PEERS"})
	if err != nil {
		return 0
	}
	if err := stream.Send(ctx, string(message)); err != nil {
		return 0
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(2*n.config.WSTimeout*float64(time.Second)))
	defer cancel()
	raw, err := stream.Recv(callCtx)
	if err != nil {
		log.Printf("Peer list request to %s failed: %v", PeerLabel(peer), err)
		return 0
	}
	data, err := decodeObject(raw)
	if err != nil {
		return 0
	}
	if messageType, _ := data["type"].(string); messageType != "PEERS" {
		return 0
	}
	return n.handlePeersMessage(data["peers"])
}

// handlePeersMessage merges a PEERS reply: every record is verified by
// addPeer (chain binding, freshness, signature, self/dedup/MaxPeers), self
// entries already in the table are pruned and the selection is refreshed.
// The list is truncated to twice SAN_MAX_PEERS so a hostile reply cannot
// force unbounded signature verification.
func (n *Node) handlePeersMessage(peers any) int {
	limit := n.config.MaxPeers * 2
	if limit < 8 {
		limit = 8
	}
	switch typed := peers.(type) {
	case []any:
		if len(typed) > limit {
			peers = typed[:limit]
		}
	case []map[string]any:
		if len(typed) > limit {
			peers = append([]map[string]any{}, typed[:limit]...)
		}
	}
	added := n.mergePeers(peers)
	removed := n.pruneSelfPeers()
	if added > 0 || removed > 0 {
		n.refreshPeerSelection()
	}
	if added > 0 {
		log.Printf("Learned %d peer(s) from a PEERS reply", added)
	}
	return added
}

// pruneSelfPeers drops records that describe this node itself (e.g. a stale
// record echoed back through a peer list). isSelf runs outside n.mu because
// it may resolve the local hostname.
func (n *Node) pruneSelfPeers() int {
	n.mu.Lock()
	peers := append([]map[string]any{}, n.PEERS...)
	n.mu.Unlock()

	selfKeys := map[string]struct{}{}
	for _, peer := range peers {
		if n.isSelf(peer) {
			selfKeys[stringValue(peer["host"])+":"+stringValue(peer["api_port"])] = struct{}{}
		}
	}
	if len(selfKeys) == 0 {
		return 0
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	kept := make([]map[string]any, 0, len(n.PEERS))
	removed := 0
	for _, peer := range n.PEERS {
		if _, self := selfKeys[stringValue(peer["host"])+":"+stringValue(peer["api_port"])]; self {
			removed++
			continue
		}
		kept = append(kept, peer)
	}
	n.PEERS = kept
	return removed
}

// ---------------------------------------------------------------------- #
// gRPC session handlers
// ---------------------------------------------------------------------- #

// PeerSession serves one inbound gRPC session: handshake, then dispatch.
func (n *Node) PeerSession(ctx context.Context, stream *PeerStream) error {
	defer stream.Close()
	if !n.acceptHandshake(ctx, stream) {
		return nil
	}
	messageCount := 0
	windowStart := time.Now()
	for {
		raw, err := stream.Recv(ctx)
		if err != nil {
			return nil
		}
		now := time.Now()
		if now.Sub(windowStart).Seconds() > n.config.PeerRateWindow {
			windowStart = now
			messageCount = 0
		}
		messageCount++
		if messageCount > n.config.PeerRateLimit {
			log.Printf("Peer exceeded the message rate limit; closing")
			stream.Close()
			return nil
		}
		if err := n.dispatchPeerMessage(ctx, stream, raw); err != nil {
			log.Printf("Peer message failed: %v", err)
		}
	}
}

// acceptHandshake requires a valid HELLO as the first message.
func (n *Node) acceptHandshake(ctx context.Context, stream *PeerStream) bool {
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(n.config.WSTimeout*2*float64(time.Second)))
	defer cancel()
	raw, err := stream.Recv(callCtx)
	if err != nil {
		return false
	}
	data, err := decodeObject(raw)
	if err != nil {
		log.Printf("Handshake failed: %v", err)
		return false
	}
	if messageType, _ := data["type"].(string); messageType != "HELLO" || !n.VerifyHello(data) {
		log.Printf("Rejected connection: invalid handshake")
		stream.Close()
		return false
	}
	ack := n.HelloPayload("HELLO_ACK")
	encoded, err := encodeObject(ack)
	if err != nil {
		return false
	}
	if err := stream.Send(ctx, string(encoded)); err != nil {
		return false
	}
	return true
}

// dispatchPeerMessage routes a message to the handler for its type.
func (n *Node) dispatchPeerMessage(ctx context.Context, stream *PeerStream, raw string) error {
	data, err := decodeObject(raw)
	if err != nil {
		return err
	}
	messageType, _ := data["type"].(string)
	switch messageType {
	case "GET_BLOCK":
		n.serveBlockRequest(ctx, stream, data["block_hash"])
		return nil
	case "BLOCK":
		n.handleIncomingBlock(ctx, data)
		return nil
	case "BLOCK_VOTE_REQUEST":
		n.serveBlockVote(ctx, stream, data)
		return nil
	}
	return n.handlePeerMessage(ctx, stream, raw)
}

// handlePeerMessage handles the non-block message types.
func (n *Node) handlePeerMessage(ctx context.Context, stream *PeerStream, raw string) error {
	data, err := decodeObject(raw)
	if err != nil {
		return err
	}
	messageType, _ := data["type"].(string)

	switch messageType {
	case "PING":
		return stream.Send(ctx, `{"type":"PONG"}`)

	case "PEER_UPDATE":
		newPeer, ok := CompletePeer(data["peer"], n.config)
		if !ok || n.isSelf(newPeer) {
			return nil
		}
		if !n.VerifyPeerRecord(newPeer) {
			return nil
		}
		if n.seenRecently(newPeer) {
			return nil
		}
		if n.addPeer(newPeer) > 0 {
			n.refreshPeerSelection()
			log.Printf("New peer learned: %s", PeerLabel(newPeer))
			n.gossipPeers(ctx, newPeer)
		}
		return nil

	case "DEAD_PEER":
		// Unsigned eviction claims are ignored: peers are only dropped by our
		// own health checks.
		log.Printf("Ignoring remote DEAD_PEER claim: %v", data["peer"])
		return nil

	case "GET_PEERS":
		message, err := encodeObject(map[string]any{"type": "PEERS", "peers": n.Peers()})
		if err != nil {
			return err
		}
		return stream.Send(ctx, string(message))

	case "PEERS":
		n.handlePeersMessage(data["peers"])
		return nil

	case "BLOCK_NOT_FOUND":
		// Consumed inline by fetchBlock as the answer to GET_BLOCK. An
		// unsolicited reply has no session peer attached here, so it is only
		// logged; Python treats it as a failed fetch as well.
		log.Printf("Ignoring unsolicited BLOCK_NOT_FOUND for %.12s", stringValue(data["block_hash"]))
		return nil

	case "TX":
		n.IngestTransaction(data["tx"])
		return nil

	case "GET_TXS":
		n.mu.Lock()
		limit := len(n.transactionPool)
		if limit > 64 {
			limit = 64
		}
		txs := make([]any, 0, limit)
		for _, tx := range n.transactionPool[:limit] {
			txs = append(txs, tx.Payload)
		}
		n.mu.Unlock()
		message, err := encodeObject(map[string]any{"type": "TXS", "txs": txs})
		if err != nil {
			return err
		}
		return stream.Send(ctx, string(message))

	case "TXS":
		txs, _ := data["txs"].([]any)
		if len(txs) > 64 {
			txs = txs[:64]
		}
		for _, payload := range txs {
			n.IngestTransaction(payload)
		}
		return nil

	case "FINALITY_VOTE":
		n.incMetric("vote_messages_received")
		n.handleFinalityVote(data["vote"])
		return nil
	}
	log.Printf("Unknown peer message type: %v", messageType)
	return nil
}

// ---------------------------------------------------------------------- #
// NodeTransport surface
// ---------------------------------------------------------------------- #

// Peers returns a copy of the known peer records.
func (n *Node) Peers() []any {
	n.mu.Lock()
	defer n.mu.Unlock()
	peers := make([]any, len(n.PEERS))
	for i, peer := range n.PEERS {
		peers[i] = peer
	}
	return peers
}

// PeerStatus is the public chain status used for longest-chain selection.
func (n *Node) PeerStatus() map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	tip := n.blockchain.Tip()
	return map[string]any{
		"version":            int64(ledger.SchemaVersion),
		"chain_id":           n.chainID,
		"genesis_allocation": n.genesisAllocationFingerprint(),
		"height":             tip.Index,
		"finalized_height":   n.finalizedHeight,
		"tip_hash":           tip.CurrentBlockHash,
	}
}

// WSMaxSize returns the maximum message size.
func (n *Node) WSMaxSize() int { return n.config.WSMaxSize }
