package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/zephyraoss/haitatsu/internal/config"
)

const (
	retryAttempts     = 3
	retryInitialDelay = 250 * time.Millisecond
)

func retryTransient(ctx context.Context, op func() error) error {
	var err error
	delay := retryInitialDelay
	for attempt := 0; attempt < retryAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(delay):
			}
			delay *= 4
		}
		err = op()
		if err == nil || !transientError(err) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

func transientError(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.StatusCode < 400 || resp.StatusCode >= 500
}

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
	return retryTransient(ctx, func() error {
		_, err := c.client.PutObject(ctx, c.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: "message/rfc822"})
		return err
	})
}

func (c *Client) GetMessage(ctx context.Context, key string) ([]byte, error) {
	return c.GetObject(ctx, key)
}

func (c *Client) GetObject(ctx context.Context, key string) ([]byte, error) {
	var data []byte
	err := retryTransient(ctx, func() error {
		object, err := c.client.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
		if err != nil {
			return err
		}
		defer object.Close()
		data, err = io.ReadAll(object)
		return err
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (c *Client) GetObjectReader(ctx context.Context, key string) (io.ReadCloser, error) {
	var reader io.ReadCloser
	err := retryTransient(ctx, func() error {
		object, err := c.client.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
		if err != nil {
			return err
		}
		if _, err := object.Stat(); err != nil {
			object.Close()
			return err
		}
		reader = object
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reader, nil
}

func (c *Client) PutExportStream(ctx context.Context, key string, data io.Reader, size int64) error {
	seeker, retryable := data.(io.Seeker)
	upload := func() error {
		if retryable {
			if _, err := seeker.Seek(0, io.SeekStart); err != nil {
				return err
			}
		}
		_, err := c.client.PutObject(ctx, c.bucket, key, data, size, minio.PutObjectOptions{ContentType: "application/zip"})
		return err
	}
	if !retryable {
		return upload()
	}
	return retryTransient(ctx, upload)
}

func (c *Client) DeleteObject(ctx context.Context, key string) error {
	return retryTransient(ctx, func() error {
		return c.client.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{})
	})
}
