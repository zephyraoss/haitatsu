package storage

import (
	"context"
	"fmt"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/zephyraoss/haitatsu/internal/config"
)

type Client struct {
	client *minio.Client
	bucket string
}

func New(cfg config.S3Config) (*Client, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, err
	}
	return &Client{client: client, bucket: cfg.Bucket}, nil
}

func (c *Client) Health(ctx context.Context) error {
	found, err := c.client.BucketExists(ctx, c.bucket)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("bucket %q does not exist", c.bucket)
	}
	return nil
}
