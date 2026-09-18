package netnode

import (
	"context"
	"testing"
)

// TestGossipedPeersFeedAddrman verifies that PEERS records which pass peer
// verification are stored in the address manager with the gossip source, while
// tampered records never reach it.
func TestGossipedPeersFeedAddrman(t *testing.T) {
	local := peerTestNode(t, 18201)
	local.addrman = NewAddrManager(8, "")
	local.addrman.SetValidator(local.VerifyPeerRecord)
	remote := peerTestNode(t, 18202)

	valid := remote.SelfPeerRecord()
	tampered := deepCopyStringMap(valid)
	tampered["host"] = "10.9.9.9"

	message, err := encodeObject(map[string]any{
		"type":  "PEERS",
		"peers": []any{valid, tampered},
	})
	if err != nil {
		t.Fatalf("encodeObject: %v", err)
	}
	if err := local.dispatchPeerMessage(context.Background(), nil, string(message)); err != nil {
		t.Fatalf("dispatchPeerMessage(PEERS): %v", err)
	}

	if !local.addrman.Has(valid) {
		t.Fatalf("verified record was not stored in the address manager: %v", local.addrman.Snapshot())
	}
	if info := mustInfo(t, local.addrman, valid); info.Source != AddrSourceGossip {
		t.Fatalf("source: got %q, want %q", info.Source, AddrSourceGossip)
	}
	if local.addrman.Has(tampered) {
		t.Fatalf("tampered record was stored in the address manager")
	}
	if local.addrman.Len() != 1 {
		t.Fatalf("address manager holds %d entries, want 1", local.addrman.Len())
	}
}

// TestPeerUpdateFeedsAddrman covers the PEER_UPDATE path used by gossip.
func TestPeerUpdateFeedsAddrman(t *testing.T) {
	local := peerTestNode(t, 18203)
	local.addrman = NewAddrManager(8, "")
	local.addrman.SetValidator(local.VerifyPeerRecord)
	remote := peerTestNode(t, 18204)

	message, err := encodeObject(map[string]any{"type": "PEER_UPDATE", "peer": remote.SelfPeerRecord()})
	if err != nil {
		t.Fatalf("encodeObject: %v", err)
	}
	if err := local.dispatchPeerMessage(context.Background(), nil, string(message)); err != nil {
		t.Fatalf("dispatchPeerMessage(PEER_UPDATE): %v", err)
	}
	if !local.addrman.Has(remote.SelfPeerRecord()) {
		t.Fatalf("PEER_UPDATE record missing from the address manager")
	}
}
