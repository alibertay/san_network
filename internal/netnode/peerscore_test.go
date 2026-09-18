package netnode

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------- #
// Scoring signals and weights
// ---------------------------------------------------------------------- #

func TestPeerScoreSignalsAndWeights(t *testing.T) {
	node := newDispatchNode(t)
	record := map[string]any{"host": "10.0.0.7", "public_key": "aa"}
	key := peerIdentityKey(record)

	if score, banned := node.notePeerSignal(record, scoreSignalHealth); score != 2 || banned {
		t.Fatalf("health signal: score=%d banned=%v, want 2/false", score, banned)
	}
	if score, _ := node.notePeerSignal(record, scoreSignalValidBlock); score != 4 {
		t.Fatalf("valid block signal: score=%d, want 4", score)
	}
	if score, _ := node.notePeerSignal(record, scoreSignalValidTx); score != 5 {
		t.Fatalf("valid tx signal: score=%d, want 5", score)
	}
	if score, _ := node.notePeerSignal(record, scoreSignalDuplicate); score != 4 {
		t.Fatalf("duplicate signal: score=%d, want 4", score)
	}
	if score, _ := node.notePeerSignal(record, scoreSignalTimeout); score != 1 {
		t.Fatalf("timeout signal: score=%d, want 1", score)
	}
	if score, _ := node.notePeerSignal(record, scoreSignalMalformed); score != -4 {
		t.Fatalf("malformed signal: score=%d, want -4", score)
	}
	if score, _ := node.notePeerSignal(record, scoreSignalRateLimit); score != -24 {
		t.Fatalf("rate-limit signal: score=%d, want -24", score)
	}
	if got := node.PeerScoreOf(record); got != -24 {
		t.Fatalf("PeerScoreOf: got %d, want -24", got)
	}
	if node.peerScoreOfKey(key) != -24 {
		t.Fatalf("score key lookup mismatch")
	}
	if got := metricValue(node, "peer_penalties"); got != 4 {
		t.Fatalf("peer_penalties: got %d, want 4", got)
	}
	if _, banned := node.notePeerSignal(record, "not-a-signal"); banned {
		t.Fatalf("unknown signal banned the peer")
	}
	if got := node.PeerScoreOf(record); got != -24 {
		t.Fatalf("unknown signal changed the score: %d", got)
	}
}

func TestPeerBanExponentialCooldown(t *testing.T) {
	config := testConfig()
	config.PeerBanSeconds = 60
	config.PeerBanMaxSeconds = 300
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	record := map[string]any{"host": "10.0.0.8", "public_key": "bb"}
	key := peerIdentityKey(record)

	for i := 0; i < 10; i++ {
		node.notePeerSignal(record, scoreSignalMalformed)
	}
	if !node.peerKeyBanned(key) {
		t.Fatalf("score below the threshold did not ban the peer")
	}
	if got := node.ActivePeerBans(); got != 1 {
		t.Fatalf("ActivePeerBans: got %d, want 1", got)
	}
	bans, bannedUntil, ok := node.peerBanInfo(key)
	if !ok || bans != 1 {
		t.Fatalf("ban info: bans=%d ok=%v, want 1/true", bans, ok)
	}
	now := node.scoreClock()
	if cooldown := bannedUntil - now; cooldown < 55 || cooldown > 61 {
		t.Fatalf("first ban cooldown: %.1fs, want ~60s", cooldown)
	}

	// Expire the ban and trigger another one: the cooldown must double.
	node.mu.Lock()
	node.peerScores[key].BannedUntil = 0
	node.mu.Unlock()
	node.notePeerSignal(record, scoreSignalMalformed)
	bans, bannedUntil, _ = node.peerBanInfo(key)
	if bans != 2 {
		t.Fatalf("second ban count: got %d, want 2", bans)
	}
	if cooldown := bannedUntil - node.scoreClock(); cooldown < 115 || cooldown > 121 {
		t.Fatalf("second ban cooldown: %.1fs, want ~120s", cooldown)
	}
}

func TestBannedPeerIsRefusedByAddPeer(t *testing.T) {
	node := newDispatchNode(t)
	record := addrRecord("banned.example", 9000)
	key := peerIdentityKey(record)
	for i := 0; i < 10; i++ {
		node.notePeerSignalKey(key, scoreSignalRateLimit)
	}
	if !node.peerKeyBanned(key) {
		t.Fatalf("setup did not ban the peer")
	}
	before := metricValue(node, "peers_rejected_banned")
	if added := node.AddPeer(record); added != 0 {
		t.Fatalf("banned peer was admitted")
	}
	if got := metricValue(node, "peers_rejected_banned"); got != before+1 {
		t.Fatalf("peers_rejected_banned: got %d, want %d", got, before+1)
	}
}

func TestScoreTableStaysBounded(t *testing.T) {
	config := testConfig()
	config.MaxPeers = 2
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	for i := 0; i < 500; i++ {
		node.notePeerSignalKey("fake-"+string(rune('a'+i%26))+"-"+PeerLabel(map[string]any{"host": "h", "api_port": int64(i)}), scoreSignalDuplicate)
	}
	if got := len(node.scoreSummary()); got > 64 {
		t.Fatalf("score table grew to %d entries, want <= 64", got)
	}
}

// ---------------------------------------------------------------------- #
// Inbound caps
// ---------------------------------------------------------------------- #

func TestInboundCapsPerIPAndSubnet(t *testing.T) {
	config := testConfig()
	config.MaxInboundPerIP = 2
	config.MaxInboundPerSubnet = 3
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	if !node.AdmitInboundPeer("127.0.0.1:1001") || !node.AdmitInboundPeer("127.0.0.1:1002") {
		t.Fatalf("first two inbound sessions must be admitted")
	}
	before := metricValue(node, "peers_rejected_inbound")
	if node.AdmitInboundPeer("127.0.0.1:1003") {
		t.Fatalf("third inbound session from one IP was admitted (cap 2)")
	}
	if got := metricValue(node, "peers_rejected_inbound"); got != before+1 {
		t.Fatalf("peers_rejected_inbound: got %d, want %d", got, before+1)
	}
	node.ReleaseInboundPeer("127.0.0.1:1001")
	if !node.AdmitInboundPeer("127.0.0.1:1004") {
		t.Fatalf("a released inbound slot was not reusable")
	}
	perIP, _ := node.inboundUsage()
	if perIP["127.0.0.1"] != 2 {
		t.Fatalf("per-IP usage: got %d, want 2", perIP["127.0.0.1"])
	}

	subnetConfig := testConfig()
	subnetConfig.MaxInboundPerIP = -1 // disable the per-IP cap
	subnetConfig.MaxInboundPerSubnet = 2
	subnetNode, err := NewNode(subnetConfig, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode(subnet): %v", err)
	}
	if !subnetNode.AdmitInboundPeer("10.0.0.1:1") || !subnetNode.AdmitInboundPeer("10.0.0.2:1") {
		t.Fatalf("first two subnet sessions must be admitted")
	}
	if subnetNode.AdmitInboundPeer("10.0.0.3:1") {
		t.Fatalf("third /24 inbound session was admitted (cap 2)")
	}
	if !subnetNode.AdmitInboundPeer("10.0.1.1:1") {
		t.Fatalf("a different /24 must not be affected by the subnet cap")
	}
}

func TestInboundCapWiredOverGRPC(t *testing.T) {
	ports := freePorts(t, 4)
	config := testConfig()
	config.APIPort = ports[0]
	config.PeerPort = ports[1]
	config.P2PPort = ports[2]
	config.ControllerPort = ports[3]
	config.MaxInboundPerIP = 1
	config.MaxInboundPerSubnet = 8
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	if err := node.Start(context.Background()); err != nil {
		t.Skipf("cannot bind gRPC ports: %v", err)
	}
	defer node.Stop()

	client, err := NewNode(testConfig(), mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode(client): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first, err := OpenSession(ctx, client, node.SelfPeerRecord(), "peer_port", 3*time.Second)
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	defer first.Close()

	if _, err := OpenSession(ctx, client, node.SelfPeerRecord(), "peer_port", 3*time.Second); err == nil {
		t.Fatalf("second session from the same IP was admitted despite the cap")
	}
	first.Close()
	waitFor(t, 3*time.Second, "inbound slot release", func() bool {
		perIP, _ := node.inboundUsage()
		return perIP["127.0.0.1"] == 0
	})
}

// ---------------------------------------------------------------------- #
// Peer-table and outbound diversity
// ---------------------------------------------------------------------- #

func TestSubnetPeerTableCap(t *testing.T) {
	config := testConfig()
	config.RequireBlockSig = false
	config.MaxPeersPerSubnet = 2
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	for index := 0; index < 2; index++ {
		record := map[string]any{
			"chain_id": node.chainID,
			"host":     "10.1.2.3",
			"api_port": int64(19000 + index),
		}
		if added := node.AddPeer(record); added != 1 {
			t.Fatalf("record %d was not admitted", index)
		}
	}
	third := map[string]any{
		"chain_id": node.chainID,
		"host":     "10.1.2.3",
		"api_port": int64(19002),
	}
	if added := node.AddPeer(third); added != 0 {
		t.Fatalf("third peer from one /24 was admitted despite the cap")
	}
	if got := metricValue(node, "peers_rejected_subnet"); got != 1 {
		t.Fatalf("peers_rejected_subnet: got %d, want 1", got)
	}
	other := map[string]any{
		"chain_id": node.chainID,
		"host":     "10.1.3.1",
		"api_port": int64(19003),
	}
	if added := node.AddPeer(other); added != 1 {
		t.Fatalf("a different /24 was rejected")
	}
}

func TestSelectOutboundPlanKeepsSubnetDiversity(t *testing.T) {
	node := newDispatchNode(t)
	node.config.MaxOutboundPerSubnet = 1
	plan := node.selectOutboundPlan([]map[string]any{
		addrRecord("10.0.0.1", 9000),
		addrRecord("10.0.0.2", 9000),
		addrRecord("10.0.1.1", 9000),
	})
	if len(plan) != 3 {
		t.Fatalf("plan dropped candidates: %d", len(plan))
	}
	if stringValue(plan[0]["host"]) != "10.0.0.1" || stringValue(plan[1]["host"]) != "10.0.1.1" ||
		stringValue(plan[2]["host"]) != "10.0.0.2" {
		t.Fatalf("diversity order wrong: %v", plan)
	}
}

// ---------------------------------------------------------------------- #
// Peer-cache reputation persistence
// ---------------------------------------------------------------------- #

func TestAddrmanPersistsPeerScore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers-cache.json")
	manager := NewAddrManager(8, path)
	record := addrRecord("scored.example", 9000)
	if !manager.Add(record, AddrSourceGossip) {
		t.Fatalf("record was not added")
	}
	manager.SetScore(record, 17)
	if err := manager.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := NewAddrManager(8, path)
	if loaded, err := reloaded.Load(); err != nil || loaded != 1 {
		t.Fatalf("Load: %d %v", loaded, err)
	}
	info, ok := reloaded.info(record)
	if !ok {
		t.Fatalf("record missing after reload")
	}
	if info.Score != 17 {
		t.Fatalf("persisted score: got %d, want 17", info.Score)
	}

	// Higher-scored tried entries are selected first.
	other := addrRecord("plain.example", 9000)
	if !reloaded.Add(other, AddrSourceGossip) {
		t.Fatalf("second record was not added")
	}
	reloaded.MarkTried(record)
	reloaded.MarkTried(other)
	now := nowSeconds() + 10
	reloaded.now = func() float64 { return now }
	selection := reloaded.Select(2)
	if len(selection) != 2 || stringValue(selection[0]["host"]) != "scored.example" {
		t.Fatalf("score did not influence selection: %v", selection)
	}
}

func TestAddrManagerScoreChangeMarksDirty(t *testing.T) {
	manager := NewAddrManager(4, filepath.Join(t.TempDir(), "peers-cache.json"))
	record := addrRecord("score.example", 9000)
	manager.Add(record, AddrSourceGossip)
	if err := manager.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if manager.Dirty() {
		t.Fatalf("manager dirty after save")
	}
	manager.SetScore(record, 5)
	if !manager.Dirty() {
		t.Fatalf("SetScore did not mark the manager dirty")
	}
}
