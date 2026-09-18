package netnode

import "testing"

// A VPS node advertises a public DNS name, so its own registry record never
// looks like a loopback address. The node must still recognise the record as
// its own and never add itself as a peer (otherwise it dials itself and
// consumes an outbound slot).
func TestAdvertisedHostSelfRecordIsNotAPeer(t *testing.T) {
	config := testConfig()
	config.AdvertiseHost = stringPointer("node1.example.com")
	config.APIPort = 18000
	config.P2PPort = 18765
	config.PeerPort = 18770
	config.ControllerPort = 18769
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	record := node.SelfPeerRecord()
	if host, _ := record["host"].(string); host != "node1.example.com" {
		t.Fatalf("self record host = %q, want node1.example.com", host)
	}
	if added := node.addPeerFrom(record, AddrSourceRegistry); added != 0 {
		t.Fatalf("the node added its own advertised record as a peer")
	}
	if len(node.PEERS) != 0 {
		t.Fatalf("peer table is not empty after adding the self record")
	}
	if !node.isSelf(record) {
		t.Fatalf("isSelf did not recognise the advertised host")
	}
}
