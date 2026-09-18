package netnode

import (
	"context"
	"log"
	"time"
)

// Outbound connection management, simplified from Bitcoin's outbound slots:
// the node keeps up to SAN_OUTBOUND_PEERS candidates from the address manager
// connected, retries failures with exponential backoff and evicts addresses
// after maxAddrFailures consecutive errors.

const (
	// outboundInterval bounds how often a dial attempt is made.
	outboundInterval = 2 * time.Second
	// addrmanSaveInterval bounds how often the address book is flushed.
	addrmanSaveInterval = 60 * time.Second
	// outboundDialTimeout bounds a single outbound attempt.
	outboundDialTimeout = 8 * time.Second
)

// outboundDialFunc attempts one outbound connection to an address; on success
// it merges whatever signed records it learned. It is a Node field so tests
// can substitute a stub dialer.
type outboundDialFunc func(ctx context.Context, record map[string]any) bool

func (n *Node) outboundLoop(ctx context.Context) {
	interval := n.config.DiscoveryInterval
	if interval <= 0 || interval > 5 {
		interval = 2.0
	}
	ticker := time.NewTicker(time.Duration(interval * float64(time.Second)))
	defer ticker.Stop()
	lastSave := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					log.Printf("Outbound maintenance failed: %v", recovered)
				}
			}()
			n.maintainOutbound(ctx)
		}()
		if time.Since(lastSave) >= addrmanSaveInterval {
			lastSave = time.Now()
			n.saveAddrman()
		}
	}
}

// saveAddrman flushes the address cache when it has changes, copying the
// local peer scores into the persisted entries first so useful reputation
// survives restarts.
func (n *Node) saveAddrman() {
	if n.addrman == nil {
		return
	}
	n.mu.Lock()
	for _, peer := range n.PEERS {
		key := peerIdentityKey(peer)
		if state, ok := n.peerScores[key]; ok {
			n.addrman.SetScore(peer, state.Score)
		}
	}
	n.mu.Unlock()
	if !n.addrman.Dirty() {
		return
	}
	if err := n.addrman.Save(); err != nil {
		log.Printf("Cannot save the peer cache: %v", err)
	}
}

// maintainOutbound dials address-manager candidates until the outbound target
// is reached, marking successes (tried) and failures (backoff/eviction).
func (n *Node) maintainOutbound(ctx context.Context) {
	if n.addrman == nil || n.config.OutboundPeers <= 0 {
		return
	}
	n.mu.Lock()
	known := make(map[string]bool, len(n.PEERS))
	for _, peer := range n.PEERS {
		known[AddrKey(peer)] = true
	}
	peerCount := len(n.PEERS)
	n.mu.Unlock()
	if peerCount >= n.config.OutboundPeers {
		return
	}
	needed := n.config.OutboundPeers - peerCount
	// Over-select so self/duplicate entries do not waste the budget, then cap
	// how many slots one subnet may occupy (eclipse resistance).
	candidates := n.addrman.Select(needed * 3)
	candidates = n.selectOutboundPlan(candidates)
	attempts := 0
	for _, record := range candidates {
		if attempts >= needed {
			break
		}
		key := AddrKey(record)
		if known[key] {
			continue
		}
		known[key] = true
		attempts++
		retrying := false
		if entry, ok := n.addrman.info(record); ok && entry.Failures > 0 {
			retrying = true
		}
		n.addrman.MarkTried(record)
		if n.dialOutbound(ctx, record) {
			n.addrman.MarkSuccess(record)
			n.notePeerSignal(record, scoreSignalHealth)
			if retrying {
				n.incMetric("peers_reconnects")
			}
			continue
		}
		if n.addrman.MarkFailure(record) {
			log.Printf("Addrman: evicted %s after %d failed attempt(s)", PeerLabel(record), maxAddrFailures)
			n.removePeer(record)
		}
	}
}

func (n *Node) dialOutbound(ctx context.Context, record map[string]any) bool {
	dial := n.outboundDial
	if dial == nil {
		dial = n.defaultOutboundDial
	}
	callCtx, cancel := context.WithTimeout(ctx, outboundDialTimeout)
	defer cancel()
	return dial(callCtx, record)
}

// defaultOutboundDial asks the candidate for its peer list over the P2P
// transport (Bootstrap RPC; the reply includes the peer's own signed record),
// falling back to a plain session handshake plus GET_PEERS/PEER_UPDATE.
func (n *Node) defaultOutboundDial(ctx context.Context, record map[string]any) bool {
	portKey := "peer_port"
	if int64Value(record["peer_port"]) == 0 {
		portKey = "p2p_port"
	}
	peers, err := remoteBootstrapPeer(ctx, n, record, portKey, 5*time.Second)
	if err == nil {
		if len(peers) > 0 {
			n.AddPeers(peers)
		} else {
			n.announceSelf(ctx, record, portKey)
		}
		n.mu.Lock()
		hasPeers := len(n.PEERS) > 0
		n.mu.Unlock()
		if hasPeers {
			n.requestPeers(ctx)
			n.requestSync(ctx)
		}
		return true
	}
	stream, sessionErr := OpenSession(ctx, n, record, portKey, n.sessionTimeout())
	if sessionErr != nil {
		log.Printf("Outbound attempt to %s failed: %v", PeerLabel(record), sessionErr)
		return false
	}
	defer stream.Close()
	message, encodeErr := encodeObject(map[string]any{"type": "GET_PEERS"})
	if encodeErr == nil {
		if sendErr := stream.Send(ctx, string(message)); sendErr == nil {
			callCtx, cancel := context.WithTimeout(ctx, time.Duration(2*n.config.WSTimeout*float64(time.Second)))
			if raw, recvErr := stream.Recv(callCtx); recvErr == nil {
				if data, decodeErr := decodeObject(raw); decodeErr == nil {
					if messageType, _ := data["type"].(string); messageType == "PEERS" {
						n.handlePeersMessage(data["peers"])
					}
				}
			}
			cancel()
		}
	}
	if selfMessage, err := encodeObject(map[string]any{"type": "PEER_UPDATE", "peer": n.SelfPeerRecord()}); err == nil {
		_ = stream.Send(ctx, string(selfMessage))
	}
	log.Printf("Outbound connection to %s established (session fallback)", PeerLabel(record))
	return true
}

// announceSelf sends this node's signed record over an already-open session.
func (n *Node) announceSelf(ctx context.Context, record map[string]any, portKey string) {
	stream, err := OpenSession(ctx, n, record, portKey, n.sessionTimeout())
	if err != nil {
		return
	}
	defer stream.Close()
	message, err := encodeObject(map[string]any{"type": "PEER_UPDATE", "peer": n.SelfPeerRecord()})
	if err != nil {
		return
	}
	_ = stream.Send(ctx, string(message))
}
