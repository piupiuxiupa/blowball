package minio

import (
	"github.com/lush/blowball/services/officevers/internal/config"

	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Client wraps a MinIO client together with the bucket name so that storage
// operations do not have to thread the bucket through every call.
type Client struct {
	raw    *miniogo.Client
	Bucket string
}

// New constructs a MinIO client from the supplied configuration.
func New(cfg config.Config) (*Client, error) {
	c, err := miniogo.New(cfg.MinIOEndpoint, &miniogo.Options{
		Creds:  credentials.NewStaticV4(cfg.MinIOAccessKey, cfg.MinIOSecretKey, ""),
		Secure: cfg.MinIOUseSSL,
		Region: cfg.MinIORegion,
	})
	if err != nil {
		return nil, err
	}
	return &Client{raw: c, Bucket: cfg.MinIOBucket}, nil
}
