package s3adapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// maxFenceBytes bounds a fence, marker or authorization object, which holds one
// decimal epoch, operation identifier or hex digest.
const maxFenceBytes = 128

// S3Config names the store, its two buckets and the adapter's own key.
type S3Config struct {
	// Endpoint is where the adapter reaches the store.
	Endpoint string
	// PublicEndpoint is where the runtime reaches it. Presigned URLs sign
	// their host, so they are built for this endpoint. It defaults to Endpoint.
	PublicEndpoint               string
	Region                       string
	AccessKey, SecretKey         string
	StagingBucket, ObjectsBucket string
	HTTPClient                   *http.Client
}

// S3Store implements ObjectStore with the AWS SDK.
type S3Store struct {
	cfg     S3Config
	client  *s3.Client
	presign *s3.PresignClient
}

func NewS3Store(cfg S3Config) (*S3Store, error) {
	if cfg.PublicEndpoint == "" {
		cfg.PublicEndpoint = cfg.Endpoint
	}
	for _, endpoint := range []string{cfg.Endpoint, cfg.PublicEndpoint} {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("S3 endpoints must be plain HTTPS origins")
		}
	}
	if cfg.Region == "" || cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.StagingBucket == "" || cfg.ObjectsBucket == "" || cfg.StagingBucket == cfg.ObjectsBucket {
		return nil, errors.New("S3 store needs a region, a key and two distinct buckets")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: time.Minute}
	}
	options := func(endpoint string) s3.Options {
		return s3.Options{
			Region:       cfg.Region,
			BaseEndpoint: aws.String(endpoint),
			UsePathStyle: true,
			HTTPClient:   cfg.HTTPClient,
			Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
			// The adapter hashes bytes itself (ADR-0017). Checksum headers the
			// SDK would add are not needed and broke presigned requests in
			// aws-sdk-go-v2 1.43.7.
			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
			ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		}
	}
	return &S3Store{cfg: cfg, client: s3.New(options(cfg.Endpoint)), presign: s3.NewPresignClient(s3.New(options(cfg.PublicEndpoint)))}, nil
}

func (s *S3Store) PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (string, error) {
	request, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.cfg.StagingBucket), Key: aws.String(key), ContentType: aws.String(contentType), ContentLength: aws.Int64(size)}, func(o *s3.PresignOptions) { o.Expires = ttl })
	if err != nil {
		return "", err
	}
	return request.URL, nil
}

func (s *S3Store) HeadStaging(ctx context.Context, key string) (Object, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.cfg.StagingBucket), Key: aws.String(key)})
	if err != nil {
		return Object{}, classify(err)
	}
	return Object{Version: aws.ToString(out.VersionId), ETag: aws.ToString(out.ETag), Size: aws.ToInt64(out.ContentLength), Metadata: out.Metadata}, nil
}

func (s *S3Store) GetStaging(ctx context.Context, key, version string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.cfg.StagingBucket), Key: aws.String(key), VersionId: aws.String(version)})
	if err != nil {
		return nil, classify(err)
	}
	return out.Body, nil
}

func (s *S3Store) GetFence(ctx context.Context, key string) ([]byte, string, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.cfg.StagingBucket), Key: aws.String(key)})
	if err != nil {
		return nil, "", classify(err)
	}
	defer out.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(out.Body, maxFenceBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) > maxFenceBytes {
		return nil, "", errors.New("artifact fence is oversized")
	}
	return raw, aws.ToString(out.ETag), nil
}

func (s *S3Store) PutFence(ctx context.Context, key string, body []byte, w Write) error {
	in := &s3.PutObjectInput{Bucket: aws.String(s.cfg.StagingBucket), Key: aws.String(key), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))), ContentType: aws.String("text/plain")}
	switch {
	case w.Create && w.Match == "":
		in.IfNoneMatch = aws.String("*")
	case !w.Create && w.Match != "":
		in.IfMatch = aws.String(w.Match)
	default:
		return errors.New("a fence write needs exactly one precondition")
	}
	_, err := s.client.PutObject(ctx, in)
	return classify(err)
}

func (s *S3Store) Freeze(ctx context.Context, stagingKey, version, objectKey string, metadata map[string]string) (Object, error) {
	source := s.cfg.StagingBucket + "/" + stagingKey + "?versionId=" + url.QueryEscape(version)
	out, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(s.cfg.ObjectsBucket), Key: aws.String(objectKey), CopySource: aws.String(source), IfNoneMatch: aws.String("*"), MetadataDirective: types.MetadataDirectiveReplace, Metadata: metadata})
	if err != nil {
		return Object{}, classify(err)
	}
	if aws.ToString(out.VersionId) == "" {
		return Object{}, errors.New("objects bucket returned no version")
	}
	return Object{Version: aws.ToString(out.VersionId)}, nil
}

// FirstObject finds the oldest version of key. S3 lists the versions of a key
// newest first, so the last one listed is the oldest; neither LastModified
// (one-second resolution) nor the shape of a version id is used.
func (s *S3Store) FirstObject(ctx context.Context, key string) (Object, error) {
	in := &s3.ListObjectVersionsInput{Bucket: aws.String(s.cfg.ObjectsBucket), Prefix: aws.String(key)}
	oldest := ""
	for {
		out, err := s.client.ListObjectVersions(ctx, in)
		if err != nil {
			return Object{}, classify(err)
		}
		for _, v := range out.Versions {
			if aws.ToString(v.Key) == key {
				oldest = aws.ToString(v.VersionId)
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		in.KeyMarker, in.VersionIdMarker = out.NextKeyMarker, out.NextVersionIdMarker
	}
	if oldest == "" {
		return Object{}, ErrNotFound
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.cfg.ObjectsBucket), Key: aws.String(key), VersionId: aws.String(oldest)})
	if err != nil {
		return Object{}, classify(err)
	}
	return Object{Version: oldest, ETag: aws.ToString(out.ETag), Size: aws.ToInt64(out.ContentLength), Metadata: out.Metadata}, nil
}

// Preflight refuses a store whose buckets cannot give the guarantees the
// adapter relies on, as far as the adapter's own key may read them. Object
// Lock needs the operator's key and is verified by Provision.
func (s *S3Store) Preflight(ctx context.Context) error {
	for _, bucket := range []string{s.cfg.StagingBucket, s.cfg.ObjectsBucket} {
		if err := s.versioned(ctx, bucket); err != nil {
			return err
		}
	}
	return s.expires(ctx, 0)
}

func (s *S3Store) versioned(ctx context.Context, bucket string) error {
	v, err := s.client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(bucket)})
	if err != nil {
		return fmt.Errorf("read versioning of %s: %w", bucket, err)
	}
	if v.Status != types.BucketVersioningStatusEnabled {
		return fmt.Errorf("bucket %s is not versioned", bucket)
	}
	return nil
}

// expires requires a rule over the whole staging bucket. Days of zero accepts
// any positive expiry, for the adapter key, which does not know the retention.
func (s *S3Store) expires(ctx context.Context, days int32) error {
	life, err := s.client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(s.cfg.StagingBucket)})
	if err != nil {
		return fmt.Errorf("read lifecycle of %s: %w", s.cfg.StagingBucket, err)
	}
	for _, rule := range life.Rules {
		if expiresEverything(rule, days) {
			return nil
		}
	}
	return fmt.Errorf("bucket %s has no expiry rule for current and noncurrent versions", s.cfg.StagingBucket)
}

func expiresEverything(rule types.LifecycleRule, days int32) bool {
	if rule.Status != types.ExpirationStatusEnabled || rule.Expiration == nil || rule.NoncurrentVersionExpiration == nil || aws.ToString(rule.Prefix) != "" {
		return false
	}
	if f := rule.Filter; f != nil && (aws.ToString(f.Prefix) != "" || f.Tag != nil || f.And != nil || f.ObjectSizeGreaterThan != nil || f.ObjectSizeLessThan != nil) {
		return false
	}
	current, noncurrent := aws.ToInt32(rule.Expiration.Days), aws.ToInt32(rule.NoncurrentVersionExpiration.NoncurrentDays)
	if days != 0 {
		return current == days && noncurrent == days
	}
	return current > 0 && noncurrent > 0
}

// classify maps the store's refusals onto the adapter's errors. A 409
// ConditionalRequestConflict is a lost race, retried like a 412.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NotFound", "NoSuchKey", "NoSuchVersion":
			return ErrNotFound
		case "PreconditionFailed", "ConditionalRequestConflict":
			return ErrPrecondition
		}
	}
	var response *awshttp.ResponseError
	if errors.As(err, &response) {
		switch response.HTTPStatusCode() {
		case http.StatusNotFound:
			return ErrNotFound
		case http.StatusPreconditionFailed, http.StatusConflict:
			return ErrPrecondition
		}
	}
	return err
}
