package netnode

import (
	"context"
	"testing"
	"time"
)

// TestDedupeControllers pins the duplicate-controller rule without network.
func TestDedupeControllers(t *testing.T) {
	records := []map[string]any{
		nil,
		{"public_key": "aa", "host": "a", "peer_port": int64(1)},
		{"public_key": "aa", "host": "b", "peer_port": int64(2)},
		{"public_key": "bb", "host": "c", "peer_port": int64(3)},
		{"host": "d", "peer_port": int64(4)},
		{"host": "d", "peer_port": int64(4)},
	}
	unique := dedupeControllers(records)
	if len(unique) != 3 {
		t.Fatalf("dedupeControllers returned %d controllers, want 3", len(unique))
	}
	if unique[0]["host"] != "a" || unique[1]["host"] != "c" || unique[2]["host"] != "d" {
		t.Fatalf("dedupe kept the wrong records: %v", unique)
	}
}

// TestControllerQuorumBehavior exercises approval counting, missing
// controllers, stale controllers and duplicate records over real gRPC.
func TestControllerQuorumBehavior(t *testing.T) {
	ports := freePorts(t, 16)
	makeNode := func(offset int) *Node {
		config := testConfig()
		config.APIPort = ports[offset]
		config.PeerPort = ports[offset+1]
		config.P2PPort = ports[offset+2]
		config.ControllerPort = ports[offset+3]
		config.WSTimeout = 0.5
		config.ControllerCount = 0
		node, err := NewNode(config, mustIdentity(t))
		if err != nil {
			t.Fatalf("NewNode: %v", err)
		}
		return node
	}
	proposer := makeNode(0)
	controllerB := makeNode(4)
	controllerC := makeNode(8)
	stale := makeNode(12)

	for _, node := range []*Node{proposer, controllerB, controllerC} {
		if err := node.Start(context.Background()); err != nil {
			t.Skipf("cannot bind gRPC ports: %v", err)
		}
		defer node.Stop()
	}
	if err := stale.Start(context.Background()); err != nil {
		t.Skipf("cannot bind stale node ports: %v", err)
	}
	defer stale.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Two of two live controllers reach the quorum.
	blockOne := buildProposal(t, proposer, proposer.identity, nil, 0)
	if !proposer.sendToControllers(ctx, []map[string]any{controllerB.SelfPeerRecord(), controllerC.SelfPeerRecord()}, blockOne) {
		t.Fatalf("2/2 controllers did not reach quorum")
	}
	// Keep B and C in step with the committed block.
	commitStaged(t, proposer, blockOne)
	for _, node := range []*Node{controllerB, controllerC} {
		if !ingestBlock(t, node, blockOne) {
			t.Fatalf("controller did not accept block 1")
		}
	}

	blockTwo := buildProposal(t, proposer, proposer.identity, nil, 0)

	// One live controller and one unreachable one: below 2/3.
	deadPeer := map[string]any{
		"host":            "127.0.0.1",
		"controller_port": int64(1), // nothing listens here
	}
	if proposer.sendToControllers(ctx, []map[string]any{controllerB.SelfPeerRecord(), deadPeer}, blockTwo) {
		t.Fatalf("1/2 controllers reached quorum")
	}

	// A duplicated live controller must not be counted twice: [B, B, dead]
	// dedupes to [B, dead], still below quorum.
	duplicated := []map[string]any{
		controllerB.SelfPeerRecord(),
		deepCopyStringMap(controllerB.SelfPeerRecord()),
		deadPeer,
	}
	if proposer.sendToControllers(ctx, duplicated, blockTwo) {
		t.Fatalf("duplicate controller record inflated the approval count")
	}

	// A stale node that does not have the parent cannot approve.
	if proposer.sendToControllers(ctx, []map[string]any{stale.SelfPeerRecord()}, blockTwo) {
		t.Fatalf("stale controller approved a block it could not verify")
	}

	// Both live controllers approve the block on top of the tip.
	if !proposer.sendToControllers(ctx, []map[string]any{controllerB.SelfPeerRecord(), controllerC.SelfPeerRecord()}, blockTwo) {
		t.Fatalf("2/2 controllers did not reach quorum for block 2")
	}
}
