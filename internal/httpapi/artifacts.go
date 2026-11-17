package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dac-plane/dac-plane/internal/application"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

// Artifacts (ADR-0025) are read like the task they belong to. Their bytes
// come from the artifact store through this replica: the store presigns a
// read of the committed version, the replica fetches it from an origin the
// operator allows, and relays it while checking it against the admitted
// record.

var errArtifactMismatch = errors.New("artifact bytes differ from the admitted record")

// heldTail is how much of a download is held back until the whole body has
// been hashed, so a body whose digest differs is never delivered complete.
const heldTail = 64 << 10

// defaultStallTimeout bounds how long a download waits for the store's next
// bytes. The Surface API has no write timeout, because it serves streams, so
// without it a store that stalls after its headers would hold the request,
// both connections and their buffers until the client gave up.
const defaultStallTimeout = 30 * time.Second

// ArtifactFetcher reads presigned artifact downloads. It fetches only from
// the origins the operator lists and follows no redirect, so an artifact
// store cannot make the control plane read anything else.
type ArtifactFetcher struct {
	origins map[string]bool
	client  *http.Client
	stall   time.Duration
}

// NewArtifactFetcher accepts plain HTTPS origins, such as
// https://artifact-store:8333. Roots, when set, replace the system roots.
func NewArtifactFetcher(origins []string, roots *x509.CertPool) (*ArtifactFetcher, error) {
	if len(origins) == 0 {
		return nil, errors.New("artifact downloads need at least one origin")
	}
	allowed := map[string]bool{}
	for _, raw := range origins {
		origin, err := plainOrigin(raw)
		if err != nil {
			return nil, err
		}
		allowed[origin] = true
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &ArtifactFetcher{
		stall:   defaultStallTimeout,
		origins: allowed,
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func plainOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("artifact download origin %q must be a plain HTTPS origin", raw)
	}
	return "https://" + strings.ToLower(u.Host), nil
}

// open starts reading a download. Its errors never contain the URL, which is
// a bearer capability.
func (f *ArtifactFetcher) open(ctx context.Context, download string) (*http.Response, error) {
	u, err := url.Parse(download)
	if err != nil || u.Scheme != "https" || !f.origins["https://"+strings.ToLower(u.Host)] {
		return nil, fmt.Errorf("%w: the store's download is not on an allowed origin", application.ErrArtifactUnavailable)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, download, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid download", application.ErrArtifactUnavailable)
	}
	response, err := f.client.Do(request)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("%w: reading the store: %v", application.ErrArtifactUnavailable, err)
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, fmt.Errorf("%w: the store answered HTTP %d", application.ErrArtifactUnavailable, response.StatusCode)
	}
	return response, nil
}

func (a *API) getArtifact(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	task, artifact, err := artifactPath(r)
	if err != nil {
		return err
	}
	record, err := application.GetArtifact(r.Context(), a.pool, actor, task, artifact)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, record)
	return nil
}

func (a *API) getArtifactContent(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	task, artifact, err := artifactPath(r)
	if err != nil {
		return err
	}
	if a.options.ArtifactDownloads == nil {
		// Not found stays not found: the record is read first.
		if _, err := application.GetArtifact(r.Context(), a.pool, actor, task, artifact); err != nil {
			return err
		}
		return fmt.Errorf("%w: downloads are not configured", application.ErrArtifactUnavailable)
	}
	download, err := application.AuthorizeArtifactDownload(r.Context(), a.pool, a.options.ArtifactStores, actor, task, artifact, a.options.Now())
	if err != nil {
		return err
	}
	fetch, cancel := context.WithCancel(r.Context())
	defer cancel()
	response, err := a.options.ArtifactDownloads.open(fetch, download.URL)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body := guardStall(response.Body, a.options.ArtifactDownloads.stall, cancel)
	defer body.stop()
	record := download.Artifact
	if response.ContentLength >= 0 && uint64(response.ContentLength) != record.Size {
		return fmt.Errorf("%w: the store reports %d bytes, the record %d", errArtifactMismatch, response.ContentLength, record.Size)
	}
	want, err := hex.DecodeString(strings.TrimPrefix(string(record.Digest), "sha256:"))
	if err != nil {
		return err
	}

	header := w.Header()
	header.Set("Content-Type", record.MediaType)
	header.Set("Content-Length", strconv.FormatUint(record.Size, 10))
	header.Set("Repr-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(want)+":")
	header.Set("ETag", `"`+string(record.Digest)+`"`)
	// The bytes are agent output: never rendered in this origin.
	header.Set("Content-Disposition", `attachment; filename="`+record.Artifact.String()+`"`)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	header.Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)

	if err := relayChecked(w, body, record, want); err != nil {
		a.logger.ErrorContext(r.Context(), "artifact download aborted", "tenant", record.Tenant.String(), "task", record.Task.String(), "artifact", record.Artifact.String(), "error", err)
		// Aborting the response tells the client the body is incomplete.
		panic(http.ErrAbortHandler)
	}
	a.logger.InfoContext(r.Context(), "artifact downloaded", "tenant", record.Tenant.String(), "task", record.Task.String(), "artifact", record.Artifact.String(), "principal", actor.Principal().ID.String(), "size", record.Size)
	return nil
}

// stallGuard cancels a download whose store sends nothing for the stall
// timeout; the cancelled read then ends the relay.
type stallGuard struct {
	body  io.Reader
	stall time.Duration
	timer *time.Timer
}

func guardStall(body io.Reader, stall time.Duration, cancel context.CancelFunc) *stallGuard {
	return &stallGuard{body: body, stall: stall, timer: time.AfterFunc(stall, cancel)}
}

func (g *stallGuard) Read(p []byte) (int, error) {
	n, err := g.body.Read(p)
	if n > 0 {
		g.timer.Reset(g.stall)
	}
	return n, err
}

func (g *stallGuard) stop() { g.timer.Stop() }

// relayChecked copies exactly the record's size, holding back the last bytes
// until the digest of the whole body is known to match.
func relayChecked(w io.Writer, body io.Reader, record lifecycle.Artifact, want []byte) error {
	hash := sha256.New()
	limited := io.LimitReader(body, int64(record.Size)+1)
	held := make([]byte, 0, 2*heldTail)
	chunk := make([]byte, 32<<10)
	var total uint64
	for {
		n, err := limited.Read(chunk)
		if n > 0 {
			total += uint64(n)
			if total > record.Size {
				return fmt.Errorf("%w: more bytes than the record", errArtifactMismatch)
			}
			hash.Write(chunk[:n])
			held = append(held, chunk[:n]...)
			if len(held) > heldTail {
				if _, err := w.Write(held[:len(held)-heldTail]); err != nil {
					return err
				}
				held = append(held[:0], held[len(held)-heldTail:]...)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading the store: %w", err)
		}
	}
	if total != record.Size {
		return fmt.Errorf("%w: %d bytes, the record %d", errArtifactMismatch, total, record.Size)
	}
	if got := hash.Sum(nil); string(got) != string(want) {
		return fmt.Errorf("%w: digest", errArtifactMismatch)
	}
	_, err := w.Write(held)
	return err
}

func artifactPath(r *http.Request) (ids.TaskID, ids.ArtifactID, error) {
	task, err := pathID[ids.Task](r, "task")
	if err != nil {
		return ids.TaskID{}, ids.ArtifactID{}, err
	}
	artifact, err := pathID[ids.Artifact](r, "artifact")
	if err != nil {
		return ids.TaskID{}, ids.ArtifactID{}, err
	}
	return task, artifact, nil
}
