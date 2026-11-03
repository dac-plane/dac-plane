// Command minimal is the smallest adapter an outside author can write, and a
// starting point for one. The release's clean-room check copies it into a module
// of its own, builds it against the published kit and runs the conformance suite.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/signal"

	"github.com/dac-plane/dac-plane/adapterkit"
	"github.com/dac-plane/dac-plane/adapterkit/wire"
)

type handler struct{}

func (handler) Send(_ context.Context, request wire.Request) (wire.Reply, error) {
	return wire.Reply{
		Operation:  request.Operation,
		Delivery:   request.Delivery,
		Outcome:    wire.OutcomeOK,
		Durability: wire.DurabilityAccepted,
		Result:     json.RawMessage(`{}`),
	}, nil
}

func (handler) Cancel(context.Context, wire.Cancel) error { return nil }

func (handler) Subscribe(context.Context, wire.Cursor) (adapterkit.Stream, error) {
	return nil, errors.New("this adapter serves no streams")
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	roots := x509.NewCertPool()
	if pem, err := os.ReadFile(os.Getenv("DAC_ADAPTER_CA_FILE")); err == nil {
		roots.AppendCertsFromPEM(pem)
	}
	err := adapterkit.Run(ctx, adapterkit.Config{
		URL:      os.Getenv("DAC_ADAPTER_URL"),
		Token:    adapterkit.FileToken(os.Getenv("DAC_ADAPTER_TOKEN_FILE")),
		Provider: os.Getenv("DAC_PROVIDER_ID"),
		Role:     wire.RoleRuntime,
		Handler:  handler{},
		TLS:      &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
