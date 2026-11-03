package s3adapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// fakeStore keeps the semantics the adapter relies on: every write makes a new
// version, versions list newest first, and conditional writes are atomic.
type fakeStore struct {
	mu       sync.Mutex
	next     int
	staging  map[string][]fakeVersion
	fences   map[string]fakeVersion
	objects  map[string][]fakeVersion
	presigns []string
	// freezeHook runs before a freeze is applied; returning an error stops it.
	freezeHook func(objectKey string) error
	// lostFreezeReply applies a freeze and then reports a transport failure.
	lostFreezeReply bool
}

type fakeVersion struct {
	id       string
	body     []byte
	metadata map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{staging: map[string][]fakeVersion{}, fences: map[string]fakeVersion{}, objects: map[string][]fakeVersion{}}
}

func (f *fakeStore) version(body []byte, metadata map[string]string) fakeVersion {
	f.next++
	return fakeVersion{id: fmt.Sprintf("v%04d", f.next), body: append([]byte(nil), body...), metadata: metadata}
}

// upload is what the runtime's PUT through a capability does.
func (f *fakeStore) upload(key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.staging[key] = append([]fakeVersion{f.version(body, nil)}, f.staging[key]...)
}

// overwrite is an unconditional PUT to an objects key, which the adapter's key
// is able to make.
func (f *fakeStore) overwrite(key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]fakeVersion{f.version(body, map[string]string{})}, f.objects[key]...)
}

func (f *fakeStore) PresignPut(_ context.Context, key, contentType string, size int64, ttl time.Duration) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	url := fmt.Sprintf("https://store.test/staging/%s?type=%s&length=%d&ttl=%s", key, contentType, size, ttl)
	f.presigns = append(f.presigns, url)
	return url, nil
}

func (f *fakeStore) HeadStaging(_ context.Context, key string) (Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	versions := f.staging[key]
	if len(versions) == 0 {
		return Object{}, ErrNotFound
	}
	return Object{Version: versions[0].id, Size: int64(len(versions[0].body))}, nil
}

func (f *fakeStore) GetStaging(_ context.Context, key, version string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.staging[key] {
		if v.id == version {
			return io.NopCloser(bytes.NewReader(v.body)), nil
		}
	}
	return nil, ErrNotFound
}

func (f *fakeStore) GetFence(_ context.Context, key string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.fences[key]
	if !ok {
		return nil, "", ErrNotFound
	}
	return v.body, v.id, nil
}

func (f *fakeStore) PutFence(_ context.Context, key string, body []byte, w Write) error {
	if len(body) > maxFenceBytes {
		return errors.New("fence exceeds what the store reads back")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.fences[key]
	if (w.Create && ok) || (!w.Create && (!ok || current.id != w.Match)) {
		return ErrPrecondition
	}
	f.fences[key] = f.version(body, nil)
	return nil
}

func (f *fakeStore) Freeze(_ context.Context, stagingKey, version, objectKey string, metadata map[string]string) (Object, error) {
	if f.freezeHook != nil {
		if err := f.freezeHook(objectKey); err != nil {
			return Object{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.objects[objectKey]) > 0 {
		return Object{}, ErrPrecondition
	}
	var source *fakeVersion
	for i, v := range f.staging[stagingKey] {
		if v.id == version {
			source = &f.staging[stagingKey][i]
		}
	}
	if source == nil {
		return Object{}, ErrNotFound
	}
	frozen := f.version(source.body, metadata)
	f.objects[objectKey] = []fakeVersion{frozen}
	if f.lostFreezeReply {
		f.lostFreezeReply = false
		return Object{}, fmt.Errorf("connection reset after freeze")
	}
	return Object{Version: frozen.id}, nil
}

func (f *fakeStore) FirstObject(_ context.Context, key string) (Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	versions := f.objects[key]
	if len(versions) == 0 {
		return Object{}, ErrNotFound
	}
	oldest := versions[len(versions)-1]
	return Object{Version: oldest.id, Size: int64(len(oldest.body)), Metadata: oldest.metadata}, nil
}

func (f *fakeStore) frozen(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	versions := f.objects[key]
	if len(versions) == 0 {
		return nil
	}
	return versions[len(versions)-1].body
}
