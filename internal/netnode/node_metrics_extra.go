package netnode

// Observability helpers added in Batch D: metric increments, byte counters,
// reorg depth tracking and the /ready lifecycle state. They live in a new
// file so the already-tested node implementation stays untouched where the
// behaviour did not change.

import (
	"time"

	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/version"
)

// addMetric adds a delta to a counter (incMetric adds one).
func (n *Node) addMetric(name string, delta int64) {
	n.metricsMu.Lock()
	n.metrics[name] += delta
	n.metricsMu.Unlock()
}

// AddBytesSent records application payload bytes written to a peer session.
func (n *Node) AddBytesSent(count int64) {
	if count > 0 {
		n.addMetric("bytes_sent", count)
	}
}

// AddBytesReceived records application payload bytes read from a peer session.
func (n *Node) AddBytesReceived(count int64) {
	if count > 0 {
		n.addMetric("bytes_received", count)
	}
}

// noteReorgLocked records the depth of the most recent reorg. The caller must
// hold n.mu (tryReorg is called with the node lock held).
func (n *Node) noteReorgLocked(depth int64) {
	if depth > n.reorgDepth {
		n.reorgDepth = depth
	}
}

// Lifecycle states reported by /ready.
const (
	StateStarting = "starting"
	StateSyncing  = "syncing"
	StateReady    = "ready"
	StateDegraded = "degraded"
)

// ReadyState computes the node lifecycle state and the individual readiness
// checks. It never performs network I/O.
func (n *Node) ReadyState() map[string]any {
	n.mu.Lock()
	running := n.running.Load()
	listeners := len(n.listeners)
	hasGenesis := len(n.blockchain.Chain) > 0
	height := n.blockchain.Tip().Index
	peers := len(n.PEERS)
	controllers := len(n.controllerNodes)
	controllerTarget := n.config.ControllerCount
	requiredControllers := n.config.effectiveControllerMinCount()
	publicDevnet := n.config.PublicDevnet
	startedAt := n.startedAt
	n.mu.Unlock()

	identityLoaded := n.identity != nil && n.identity.PublicKeyHex() != ""
	p2pUp := listeners > 0
	inFlight := n.syncInFlight.Load()

	synced := !inFlight && (peers == 0 || height > 0)
	controllersAvailable := controllers > 0
	peersAvailable := peers > 0

	checks := map[string]any{
		"database_loaded":       true, // NewNode fails before this point when the store cannot open
		"genesis_verified":      hasGenesis,
		"identity_loaded":       identityLoaded,
		"p2p_listeners_up":      p2pUp,
		"sync_in_flight":        inFlight,
		"synced":                synced,
		"peers_available":       peersAvailable,
		"controllers_available": controllersAvailable,
		"controller_target":     int64(controllerTarget),
		"required_controllers":  int64(requiredControllers),
		"public_devnet":         publicDevnet,
	}

	state := StateReady
	switch {
	case !running:
		state = StateStarting
	case !hasGenesis || !identityLoaded || !p2pUp:
		state = StateDegraded
	case publicDevnet && peers == 0:
		state = StateDegraded
	case publicDevnet && requiredControllers > 0 && controllers < requiredControllers:
		state = StateDegraded
	case inFlight || (peers > 0 && height == 0):
		state = StateSyncing
	}

	n.mu.Lock()
	n.readyState = state
	n.mu.Unlock()

	ageSeconds := 0.0
	if !startedAt.IsZero() {
		ageSeconds = time.Since(startedAt).Seconds()
	}
	info := version.Resolve(ProtocolVersion, ledger.SchemaVersion)
	return map[string]any{
		"state":        state,
		"ready":        state == StateReady,
		"updated_at":   nowSeconds(),
		"uptime_secs":  ageSeconds,
		"checks":       checks,
		"version_info": info.Map(),
	}
}
