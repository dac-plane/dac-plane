package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

// httpsUploader sends a fixture the way the Codex adapter sends a real
// artifact: one PUT to the capability with Content-Type and Content-Length and
// nothing else. The capability is never logged or kept.
func httpsUploader(tlsConfig *tls.Config) simulator.ArtifactUploader {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if tlsConfig != nil {
		transport.TLSClientConfig = tlsConfig.Clone()
	}
	client := &http.Client{Transport: transport, Timeout: time.Minute}
	return func(ctx context.Context, t contracts.ArtifactTransfer, data []byte) error {
		u, err := url.Parse(t.Upload)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return errors.New("artifact upload capability must be an HTTPS URL")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, t.Upload, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", t.Manifest.MediaType)
		req.ContentLength = int64(len(data))
		resp, err := client.Do(req)
		if err != nil {
			// The URL carries the capability; keep it out of the error.
			var u *url.Error
			if errors.As(err, &u) {
				return fmt.Errorf("artifact upload: %w", u.Err)
			}
			return errors.New("artifact upload failed")
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("artifact upload returned %s", resp.Status)
		}
		return nil
	}
}
