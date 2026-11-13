package minio

import (
	"context"
	"fmt"

	miniogo "github.com/minio/minio-go/v7"
)

// Bootstrap verifies that the configured bucket exists and has Object Versioning
// enabled. If the bucket does not exist it is created; if versioning is not
// enabled it is enabled. The service MUST fail fast if versioning cannot be
// enabled, because versions written before versioning is on cannot be recovered
// retroactively (Requirement: Bucket versioning is ensured at startup).
func (c *Client) Bootstrap(ctx context.Context) error {
	exists, err := c.raw.BucketExists(ctx, c.Bucket)
	if err != nil {
		return fmt.Errorf("check bucket %q: %w", c.Bucket, err)
	}
	if !exists {
		if err := c.raw.MakeBucket(ctx, c.Bucket, miniogo.MakeBucketOptions{}); err != nil {
			return fmt.Errorf("create bucket %q: %w", c.Bucket, err)
		}
	}

	vcfg, err := c.raw.GetBucketVersioning(ctx, c.Bucket)
	if err != nil {
		return fmt.Errorf("get bucket versioning: %w", err)
	}
	if vcfg.Status != "Enabled" {
		if err := c.raw.SetBucketVersioning(ctx, c.Bucket, miniogo.BucketVersioningConfiguration{Status: "Enabled"}); err != nil {
			return fmt.Errorf("enable bucket versioning: %w", err)
		}
	}
	return nil
}
