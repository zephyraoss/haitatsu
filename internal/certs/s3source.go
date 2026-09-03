package certs

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/zephyraoss/haitatsu/internal/config"
)

type S3Source struct {
	client *minio.Client
	bucket string
	prefix string
}

func NewS3Source(cfg config.TLSStorageConfig) (*S3Source, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("configure certificate storage: %w", err)
	}
	return &S3Source{client: client, bucket: cfg.Bucket, prefix: strings.Trim(strings.TrimSpace(cfg.Prefix), "/")}, nil
}

func (s *S3Source) Load(ctx context.Context, key string) ([]byte, error) {
	object, err := s.client.GetObject(ctx, s.bucket, path.Join(s.prefix, key), minio.GetObjectOptions{})
	if err != nil {
		return nil, translateNotFound(err)
	}
	defer object.Close()
	data, err := io.ReadAll(object)
	if err != nil {
		return nil, translateNotFound(err)
	}
	return data, nil
}

func translateNotFound(err error) error {
	resp := minio.ToErrorResponse(err)
	if resp.StatusCode == http.StatusNotFound || resp.Code == "NoSuchKey" {
		return fs.ErrNotExist
	}
	return err
}
