package bootstrap

import (
	"crypto/tls"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certificateCheckInterval bounds how often a handshake looks for replaced
// certificate files.
const certificateCheckInterval = 5 * time.Second

// certificate serves the mounted TLS key pair and reloads it when either file
// changes, so an operator replaces certificates without a restart or a new
// image (ADR-0009). A replacement that fails to load keeps the previous pair.
type certificate struct {
	certFile, keyFile string
	logger            *slog.Logger

	mu        sync.Mutex
	current   *tls.Certificate
	modified  [2]time.Time
	checkedAt time.Time
}

func loadCertificate(certFile, keyFile string, logger *slog.Logger) (*certificate, error) {
	c := &certificate{certFile: certFile, keyFile: keyFile, logger: logger}
	modified, err := c.stat()
	if err != nil {
		return nil, err
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, errors.New("cannot load the TLS certificate and key")
	}
	c.current, c.modified, c.checkedAt = &pair, modified, time.Now()
	return c, nil
}

func (c *certificate) stat() ([2]time.Time, error) {
	var modified [2]time.Time
	for i, path := range []string{c.certFile, c.keyFile} {
		info, err := os.Stat(path)
		if err != nil {
			return modified, errors.New("cannot read the TLS certificate or key file")
		}
		modified[i] = info.ModTime()
	}
	return modified, nil
}

// get is a tls.Config GetCertificate callback.
func (c *certificate) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.checkedAt) < certificateCheckInterval {
		return c.current, nil
	}
	c.checkedAt = time.Now()
	modified, err := c.stat()
	if err != nil || modified == c.modified {
		return c.current, nil
	}
	pair, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		// A half-written replacement loads on a later handshake.
		c.logger.Warn("replaced TLS certificate did not load; serving the previous one")
		return c.current, nil
	}
	c.current, c.modified = &pair, modified
	c.logger.Info("TLS certificate reloaded")
	return c.current, nil
}

// serverTLS is the configuration every TLS listener serves. nextProtos pins
// the application protocols; the adapter endpoint offers HTTP/1.1 only,
// which the WebSocket upgrade needs.
func (c *certificate) serverTLS(nextProtos ...string) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: c.get, NextProtos: nextProtos}
}
