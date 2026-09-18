package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/alibertay/san_network/internal/api"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/netnode"
)

// runChild is the detached node process spawned by the launcher. It reads the
// SAN_* environment prepared by the parent and serves until interrupted.
func runChild(stdout, stderr io.Writer) int {
	keyFile := os.Getenv("SAN_KEY_FILE")
	identity, err := ledger.LoadIdentity(keyFile, false)
	if err != nil {
		fmt.Fprintf(stderr, "sanup node: cannot load the key %s: %v\n", keyFile, err)
		return 1
	}
	config := netnode.NodeConfigFromEnv()
	node, err := netnode.NewNode(config, identity)
	if err != nil {
		fmt.Fprintf(stderr, "sanup node: cannot start the node: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := node.Start(ctx); err != nil {
		fmt.Fprintf(stderr, "sanup node: cannot start the node: %v\n", err)
		return 1
	}
	defer node.Stop()
	if err := api.Run(ctx, node, config); err != nil {
		fmt.Fprintf(stderr, "sanup node: api server failed: %v\n", err)
		return 1
	}
	return 0
}
