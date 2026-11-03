package s3adapter

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	// ErrNotFound reports an absent key or version.
	ErrNotFound = errors.New("object not found")
	// ErrPrecondition reports a refused If-None-Match or If-Match write.
	ErrPrecondition = errors.New("precondition failed")
)

// Object describes one stored version.
type Object struct {
	Version  string
	ETag     string
	Size     int64
	Metadata map[string]string
}

// Write states the precondition of a conditional put. Exactly one of Create
// and Match is set.
type Write struct {
	Create bool
	Match  string
}

// ObjectStore is the subset of the S3 API the adapter relies on. Every method
// that names a version reads exactly that version.
type ObjectStore interface {
	// PresignPut returns a URL that uploads one key with one content type
	// and length until it expires.
	PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (string, error)
	// HeadStaging reports the current version of a staging key.
	HeadStaging(ctx context.Context, key string) (Object, error)
	// GetStaging opens one staging version.
	GetStaging(ctx context.Context, key, version string) (io.ReadCloser, error)
	// GetFence reads a fence object and its ETag.
	GetFence(ctx context.Context, key string) ([]byte, string, error)
	// PutFence writes a fence object under its precondition.
	PutFence(ctx context.Context, key string, body []byte, w Write) error
	// Freeze copies one staging version to an objects key that must not exist.
	Freeze(ctx context.Context, stagingKey, version, objectKey string, metadata map[string]string) (Object, error)
	// FirstObject reports the oldest version of an objects key.
	FirstObject(ctx context.Context, key string) (Object, error)
}
