package minio

import (
	"context"

	miniogo "github.com/minio/minio-go/v7"
)

// Rollback non-destructively restores a prior version of {uuid}/{<hashed leaf>} as
// the new latest version, by performing a server-side CopyObject from the target
// version to the same key. This creates a NEW latest version whose content
// equals the target version; every existing version (including those newer than
// the target) is retained and remains retrievable.
//
// The target version is validated to exist first. Rolling back an unknown
// version or a non-existent file returns ErrNotFound without modifying anything.
// (Requirement: Non-destructive rollback to a prior version)
func (c *Client) Rollback(ctx context.Context, uuid, path, targetVersionID string) (string, error) {
	if err := ValidateUUID(uuid); err != nil {
		return "", err
	}
	name, err := StorageLeaf(path)
	if err != nil {
		return "", err
	}
	if targetVersionID == "" {
		return "", ErrEmptyVersionID
	}

	// 1. Validate the target version exists (also covers non-existent file).
	versions, err := c.ListVersions(ctx, uuid, path)
	if err != nil {
		return "", err
	}
	found := false
	for _, v := range versions {
		if v.VersionID == targetVersionID {
			found = true
			break
		}
	}
	if !found {
		return "", ErrNotFound
	}

	// 2. Server-side copy from the target version to the same key, creating a
	//    new latest version with identical content. History is preserved.
	key := ObjectKey(uuid, name)
	dst := miniogo.CopyDestOptions{Bucket: c.Bucket, Object: key}
	src := miniogo.CopySrcOptions{Bucket: c.Bucket, Object: key, VersionID: targetVersionID}
	info, err := c.raw.CopyObject(ctx, dst, src)
	if err != nil {
		if isNotFoundErr(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	return info.VersionID, nil
}
