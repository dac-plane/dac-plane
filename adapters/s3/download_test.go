package s3adapter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dac-plane/dac-plane/adapterkit/wire"
)

func downloadRequestFor(t *testing.T, m manifest, version string) wire.Request {
	t.Helper()
	raw, err := json.Marshal(downloadRequest{Manifest: m, ObjectVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Request{Operation: "019a0d5b-f74c-7000-8000-000000000120", Delivery: "delivery-download", Tenant: tenant, Attempt: attempt, Role: wire.RoleArtifactStore, Method: "authorize_download", Arguments: raw, Demand: "person"}
}

func committedArtifact(t *testing.T, store *fakeStore, a *Adapter, data []byte) (manifest, committed) {
	t.Helper()
	m := manifestFor(data, 1)
	authorize(t, a, m, "019a0d5b-f74c-7000-8000-000000000110")
	store.upload(stagingKey(m), data)
	c := commitReply(t, a, m)
	if c.Outcome != "committed" {
		t.Fatalf("commit: %+v", c)
	}
	return m, c
}

func TestDownloadPresignsTheCommittedVersionOnly(t *testing.T) {
	store := newFakeStore()
	a := newAdapter(t, store)
	m, c := committedArtifact(t, store, a, []byte("the review\n"))

	reply, err := a.Send(t.Context(), downloadRequestFor(t, m, c.ObjectVersion))
	if err != nil || reply.Outcome != wire.OutcomeOK {
		t.Fatalf("download: %v %+v", err, reply.Failure)
	}
	conform(t, "download-authorized", reply.Result)
	var d downloadAuthorized
	if err := decodeStrict(reply.Result, &d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Download, "objects/"+objectKey(m)+"?versionId="+c.ObjectVersion) || !strings.Contains(d.Download, "ttl=5m0s") {
		t.Fatalf("download is not scoped to the committed version: %s", d.Download)
	}
	if want := fixedNow.Add(defaultDownloadTTL).Truncate(time.Second); !d.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %s, want %s", d.ExpiresAt, want)
	}

	// A later overwrite of the objects key does not move what is signed.
	store.overwrite(objectKey(m), []byte("tampered"))
	again, _ := a.Send(t.Context(), downloadRequestFor(t, m, c.ObjectVersion))
	if again.Outcome != wire.OutcomeOK {
		t.Fatalf("download after overwrite: %+v", again.Failure)
	}
}

func TestDownloadRefusesWhatWasNotCommitted(t *testing.T) {
	store := newFakeStore()
	a := newAdapter(t, store)
	m, c := committedArtifact(t, store, a, []byte("the review\n"))
	other := m
	other.Digest = manifestFor([]byte("something else"), 1).Digest

	for name, tc := range map[string]struct {
		request wire.Request
		code    string
	}{
		"another version":         {downloadRequestFor(t, m, "v9999"), wire.FailureStalePrecondition},
		"another manifest":        {downloadRequestFor(t, other, c.ObjectVersion), wire.FailureStalePrecondition},
		"an uncommitted artifact": {downloadRequestFor(t, manifestFor([]byte("x"), 1), "v0001"), ""},
		"no version":              {downloadRequestFor(t, m, ""), wire.FailureInvalidArguments},
	} {
		if name == "an uncommitted artifact" {
			fresh := newAdapter(t, newFakeStore())
			reply, err := fresh.Send(t.Context(), tc.request)
			if err != nil || reply.Outcome != wire.OutcomeFailed || reply.Failure.Code != wire.FailureInvalidArguments {
				t.Fatalf("%s: %v %+v", name, err, reply)
			}
			continue
		}
		reply, err := a.Send(t.Context(), tc.request)
		if err != nil || reply.Outcome != wire.OutcomeFailed || reply.Failure.Code != tc.code {
			t.Fatalf("%s: %v %+v", name, err, reply)
		}
	}

	foreign := downloadRequestFor(t, m, c.ObjectVersion)
	foreign.Attempt = "019a0d5b-f74c-7000-8000-000000000199"
	if reply, _ := a.Send(t.Context(), foreign); reply.Outcome != wire.OutcomeFailed {
		t.Fatal("a download bound to another attempt was signed")
	}
}

func TestDownloadTTLIsBounded(t *testing.T) {
	for _, ttl := range []time.Duration{time.Millisecond, 16 * time.Minute} {
		if _, err := New(Config{Store: newFakeStore(), ProviderID: "p", DownloadTTL: ttl}); err == nil {
			t.Fatalf("download TTL %s accepted", ttl)
		}
	}
}
