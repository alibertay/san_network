package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/alibertay/san_network/internal/netnode"
)

// Run serves the API until ctx is cancelled. It mirrors app/main.py's
// uvicorn.run(app, host=config.host, port=config.api_port, workers=1) and
// enables TLS when SAN_TLS_CERT/SAN_TLS_KEY are configured.
func Run(ctx context.Context, node *netnode.Node, config netnode.NodeConfig) error {
	handler := NewServer(node, config)
	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", config.Host, config.APIPort),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	log.Printf("SAN Network API listening on http://%s (tls=%v)", listener.Addr().String(), config.TLSEnabled())

	errCh := make(chan error, 1)
	go func() {
		if config.TLSEnabled() {
			errCh <- server.ServeTLS(listener, *config.TLSCert, *config.TLSKey)
			return
		}
		errCh <- server.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
