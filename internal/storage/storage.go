package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"

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

func (c *Client) PutMessage(ctx context.Context, key string, data []byte) error {
	_, err := c.client.PutObject(ctx, c.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: "message/rfc822"})
	return err
}

func (c *Client) GetMessage(ctx context.Context, key string) ([]byte, error) {
	object, err := c.client.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer object.Close()
	return io.ReadAll(object)
}

func (c *Client) PutExport(ctx context.Context, key string, data []byte) error {
	_, err := c.client.PutObject(ctx, c.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: "application/zip"})
	return err
}

func (c *Client) DeleteObject(ctx context.Context, key string) error {
	return c.client.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{})
}
