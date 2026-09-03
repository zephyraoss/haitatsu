package certs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/zephyraoss/haitatsu/internal/config"
)

const (
	defaultS3Prefix   = "certmagic"
	lockTTL           = 2 * time.Minute
	lockPollInterval  = time.Second
	lockRefreshPeriod = lockTTL / 3
)

type S3Storage struct {
	client *minio.Client
	bucket string
	prefix string
	mu     sync.Mutex
	locks  map[string]chan struct{}
}

func NewS3Storage(cfg config.S3Config, prefix string) (*S3Storage, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("configure certificate storage: %w", err)
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		prefix = defaultS3Prefix
	}
	return &S3Storage{client: client, bucket: cfg.Bucket, prefix: prefix, locks: map[string]chan struct{}{}}, nil
}

func (s *S3Storage) objectKey(key string) string {
	return path.Join(s.prefix, key)
}

func (s *S3Storage) trimPrefix(key string) string {
	return strings.TrimPrefix(strings.TrimPrefix(key, s.prefix), "/")
}

func (s *S3Storage) Store(ctx context.Context, key string, value []byte) error {
	_, err := s.client.PutObject(ctx, s.bucket, s.objectKey(key), bytes.NewReader(value), int64(len(value)), minio.PutObjectOptions{})
	return err
}

func (s *S3Storage) Load(ctx context.Context, key string) ([]byte, error) {
	object, err := s.client.GetObject(ctx, s.bucket, s.objectKey(key), minio.GetObjectOptions{})
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

func (s *S3Storage) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, s.objectKey(key), minio.RemoveObjectOptions{})
}

func (s *S3Storage) Exists(ctx context.Context, key string) bool {
	_, err := s.client.StatObject(ctx, s.bucket, s.objectKey(key), minio.StatObjectOptions{})
	return err == nil
}

func (s *S3Storage) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	listPrefix := s.objectKey(prefix)
	if listPrefix != "" && !strings.HasSuffix(listPrefix, "/") {
		listPrefix += "/"
	}
	var keys []string
	for object := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: listPrefix, Recursive: recursive}) {
		if object.Err != nil {
			return nil, object.Err
		}
		keys = append(keys, strings.TrimSuffix(s.trimPrefix(object.Key), "/"))
	}
	if len(keys) == 0 {
		return nil, fs.ErrNotExist
	}
	return keys, nil
}

func (s *S3Storage) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	info, err := s.client.StatObject(ctx, s.bucket, s.objectKey(key), minio.StatObjectOptions{})
	if err == nil {
		return certmagic.KeyInfo{Key: key, Modified: info.LastModified, Size: info.Size, IsTerminal: true}, nil
	}
	if !isNotFound(err) {
		return certmagic.KeyInfo{}, err
	}
	children, listErr := s.List(ctx, key, false)
	if listErr != nil || len(children) == 0 {
		return certmagic.KeyInfo{}, fs.ErrNotExist
	}
	return certmagic.KeyInfo{Key: key, IsTerminal: false}, nil
}

func (s *S3Storage) Lock(ctx context.Context, name string) error {
	lockKey := s.lockKey(name)
	for {
		acquired, err := s.tryCreateLock(ctx, lockKey)
		if err != nil {
			return err
		}
		if acquired {
			s.startLockRefresh(name, lockKey)
			return nil
		}
		if err := s.removeStaleLock(ctx, lockKey); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}
}

func (s *S3Storage) Unlock(ctx context.Context, name string) error {
	s.mu.Lock()
	if stop, ok := s.locks[name]; ok {
		close(stop)
		delete(s.locks, name)
	}
	s.mu.Unlock()
	return s.client.RemoveObject(ctx, s.bucket, s.lockKey(name), minio.RemoveObjectOptions{})
}

func (s *S3Storage) String() string {
	return "S3Storage:" + s.bucket + "/" + s.prefix
}

func (s *S3Storage) lockKey(name string) string {
	return s.objectKey(path.Join("locks", certmagic.StorageKeys.Safe(name)+".lock"))
}

func (s *S3Storage) tryCreateLock(ctx context.Context, lockKey string) (bool, error) {
	opts := minio.PutObjectOptions{}
	opts.SetMatchETagExcept("*")
	if _, err := s.client.PutObject(ctx, s.bucket, lockKey, bytes.NewReader(lockPayload()), int64(len(lockPayload())), opts); err != nil {
		if isPreconditionFailed(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *S3Storage) removeStaleLock(ctx context.Context, lockKey string) error {
	info, err := s.client.StatObject(ctx, s.bucket, lockKey, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	if time.Since(info.LastModified) < lockTTL {
		return nil
	}
	return s.client.RemoveObject(ctx, s.bucket, lockKey, minio.RemoveObjectOptions{})
}

func (s *S3Storage) startLockRefresh(name, lockKey string) {
	stop := make(chan struct{})
	s.mu.Lock()
	s.locks[name] = stop
	s.mu.Unlock()
	go func() {
		ticker := time.NewTicker(lockRefreshPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), lockPollInterval*10)
				_, _ = s.client.PutObject(ctx, s.bucket, lockKey, bytes.NewReader(lockPayload()), int64(len(lockPayload())), minio.PutObjectOptions{})
				cancel()
			}
		}
	}()
}

func lockPayload() []byte {
	return []byte(strconv.FormatInt(time.Now().Unix(), 10))
}

func isNotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.StatusCode == http.StatusNotFound || resp.Code == "NoSuchKey"
}

func isPreconditionFailed(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.StatusCode == http.StatusPreconditionFailed || resp.Code == "PreconditionFailed"
}

func translateNotFound(err error) error {
	if isNotFound(err) {
		return fs.ErrNotExist
	}
	var minioErr minio.ErrorResponse
	if errors.As(err, &minioErr) && minioErr.Code == "NoSuchKey" {
		return fs.ErrNotExist
	}
	return err
}
