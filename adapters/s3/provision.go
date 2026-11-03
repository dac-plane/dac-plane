package s3adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Retention configures what Provision applies.
type Retention struct {
	// ObjectDays is the governance-mode default retention of committed objects.
	ObjectDays int32
	// StagingDays expires uploads and fences. It must outlast the product's
	// 24-hour artifact deadline plus an upload capability's lifetime.
	StagingDays int32
}

// Provision creates or completes both buckets through the S3 API with the
// operator's key, then verifies them. It is safe to run again: an existing
// bucket is configured, never recreated (seaweedfs#11450 is why buckets are
// never created with weed shell).
func (s *S3Store) Provision(ctx context.Context, r Retention) error {
	if r.ObjectDays <= 0 || r.StagingDays < 2 {
		return errors.New("retention needs positive object days and at least two staging days")
	}
	if err := s.createBucket(ctx, s.cfg.ObjectsBucket, true); err != nil {
		return err
	}
	if err := s.createBucket(ctx, s.cfg.StagingBucket, false); err != nil {
		return err
	}
	if _, err := s.client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(s.cfg.StagingBucket), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
		return fmt.Errorf("enable versioning of %s: %w", s.cfg.StagingBucket, err)
	}
	if _, err := s.client.PutObjectLockConfiguration(ctx, &s3.PutObjectLockConfigurationInput{Bucket: aws.String(s.cfg.ObjectsBucket), ObjectLockConfiguration: &types.ObjectLockConfiguration{
		ObjectLockEnabled: types.ObjectLockEnabledEnabled,
		Rule:              &types.ObjectLockRule{DefaultRetention: &types.DefaultRetention{Mode: types.ObjectLockRetentionModeGovernance, Days: aws.Int32(r.ObjectDays)}},
	}}); err != nil {
		return fmt.Errorf("set object lock of %s: %w", s.cfg.ObjectsBucket, err)
	}
	if _, err := s.client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{Bucket: aws.String(s.cfg.StagingBucket), LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{{
		ID:                          aws.String("dac-expire-staging"),
		Status:                      types.ExpirationStatusEnabled,
		Filter:                      &types.LifecycleRuleFilter{Prefix: aws.String("")},
		Expiration:                  &types.LifecycleExpiration{Days: aws.Int32(r.StagingDays)},
		NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{NoncurrentDays: aws.Int32(r.StagingDays)},
	}}}}); err != nil {
		return fmt.Errorf("set lifecycle of %s: %w", s.cfg.StagingBucket, err)
	}
	// Denying deletes to everyone keeps out the delete marker that would let a
	// second conditional copy succeed. Operator deletion removes it first.
	policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Sid":"DacNoDelete","Effect":"Deny","Principal":"*","Action":["s3:DeleteObject","s3:DeleteObjectVersion"],"Resource":["arn:aws:s3:::%s/*"]}]}`, s.cfg.ObjectsBucket)
	if _, err := s.client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(s.cfg.ObjectsBucket), Policy: aws.String(policy)}); err != nil {
		return fmt.Errorf("set policy of %s: %w", s.cfg.ObjectsBucket, err)
	}
	if err := s.Verify(ctx, r); err != nil {
		return err
	}
	return s.conditionalWrite(ctx)
}

// conditionalWrite proves the store serves the If-None-Match writes the adapter
// relies on. SeaweedFS holds them until its lock ring forms, which was observed
// about nine seconds after its S3 listener answered, so readiness is this
// write and not an HTTP answer. The probe expires with staging.
func (s *S3Store) conditionalWrite(ctx context.Context) error {
	key := fmt.Sprintf("readiness/%d", time.Now().UnixNano())
	err := s.PutFence(ctx, key, []byte("1"), Write{Create: true})
	if err != nil {
		return fmt.Errorf("conditional write to %s: %w", s.cfg.StagingBucket, err)
	}
	if err := s.PutFence(ctx, key, []byte("1"), Write{Create: true}); !errors.Is(err, ErrPrecondition) {
		return fmt.Errorf("store accepted a second create of one key: %v", err)
	}
	return nil
}

// Verify reads every setting Provision applies back with the operator's key.
func (s *S3Store) Verify(ctx context.Context, r Retention) error {
	if err := s.Preflight(ctx); err != nil {
		return err
	}
	if err := s.expires(ctx, r.StagingDays); err != nil {
		return err
	}
	lock, err := s.client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String(s.cfg.ObjectsBucket)})
	if err != nil {
		return fmt.Errorf("read object lock of %s: %w", s.cfg.ObjectsBucket, err)
	}
	c := lock.ObjectLockConfiguration
	if c == nil || c.ObjectLockEnabled != types.ObjectLockEnabledEnabled || c.Rule == nil || c.Rule.DefaultRetention == nil ||
		c.Rule.DefaultRetention.Mode != types.ObjectLockRetentionModeGovernance || aws.ToInt32(c.Rule.DefaultRetention.Days) != r.ObjectDays {
		return fmt.Errorf("bucket %s does not carry the configured governance retention", s.cfg.ObjectsBucket)
	}
	if _, err := s.client.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(s.cfg.ObjectsBucket)}); err != nil {
		return fmt.Errorf("read policy of %s: %w", s.cfg.ObjectsBucket, err)
	}
	return nil
}

func (s *S3Store) createBucket(ctx context.Context, bucket string, locked bool) error {
	in := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	if locked {
		in.ObjectLockEnabledForBucket = aws.Bool(true)
	}
	_, err := s.client.CreateBucket(ctx, in)
	var api smithy.APIError
	if err == nil || (errors.As(err, &api) && (api.ErrorCode() == "BucketAlreadyOwnedByYou" || api.ErrorCode() == "BucketAlreadyExists")) {
		return nil
	}
	return fmt.Errorf("create bucket %s: %w", bucket, err)
}
