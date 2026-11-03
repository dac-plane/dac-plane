package s3adapter

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit"
	"github.com/AssahBismarkabah/dac-plane/adapterkit/conformance"
	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
)

func TestAdapterKitConformance(t *testing.T) {
	providerID := "019a0d5b-f74c-7000-8000-000000000198"
	request := request(t, "authorize_upload", "019a0d5b-f74c-7000-8000-000000000110", manifestFor([]byte("artifact"), 1))
	fixture := conformance.Fixture{Provider: providerID, Role: wire.RoleArtifactStore, Request: request}
	conformance.Run(t, conformance.Target{Fixture: fixture, Start: func(ctx context.Context, endpoint conformance.Endpoint) (func(), error) {
		handler, err := New(Config{Store: newFakeStore(), ProviderID: providerID})
		if err != nil {
			return nil, err
		}
		runCtx, cancel := context.WithCancel(ctx)
		token := func(context.Context) (adapterkit.Token, error) {
			return adapterkit.Token{Value: "conformance-token", Expiry: time.Now().Add(time.Hour)}, nil
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = adapterkit.Run(runCtx, adapterkit.Config{URL: endpoint.URL, Token: token, Provider: fixture.Provider, Role: wire.RoleArtifactStore, Handler: handler, Offered: []string{wire.CapabilityOperationReplay}, TLS: &tls.Config{RootCAs: endpoint.RootCAs, MinVersion: tls.VersionTLS12}})
		}()
		return func() { cancel(); <-done }, nil
	}})
}
