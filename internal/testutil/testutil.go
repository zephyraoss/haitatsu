package testutil

import (
	"context"
	stdsql "database/sql"
	"fmt"
	"io"
	"sync"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
)

type FakeStore struct {
	mu      sync.Mutex
	Objects map[string][]byte
	Gets    int
}

func NewFakeStore() *FakeStore {
	return &FakeStore{Objects: map[string][]byte{}}
}

func (s *FakeStore) GetMessage(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Gets++
	data, ok := s.Objects[key]
	if !ok {
		return nil, fmt.Errorf("object %q not found", key)
	}
	return data, nil
}

func (s *FakeStore) GetObject(ctx context.Context, key string) ([]byte, error) {
	return s.GetMessage(ctx, key)
}

func (s *FakeStore) PutMessage(_ context.Context, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Objects[key] = data
	return nil
}

func (s *FakeStore) DeleteObject(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Objects, key)
	return nil
}

func (s *FakeStore) GetObjectReader(ctx context.Context, key string) (io.ReadCloser, error) {
	data, err := s.GetMessage(ctx, key)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytesReader(data)), nil
}

func (s *FakeStore) PutExportStream(ctx context.Context, key string, data io.Reader, _ int64) error {
	buf, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	return s.PutMessage(ctx, key, buf)
}

func NewClient(t testing.TB) (*ent.Client, *stdsql.DB) {
	t.Helper()
	db, err := stdsql.Open("sqlite", fmt.Sprintf("file:%s?mode=memory&cache=shared&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", sanitize(t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.SQLite, db)))
	t.Cleanup(func() { client.Close() })
	if err := client.Schema.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	return client, db
}

func NewMailStore(t testing.TB, client *ent.Client) *mailstore.Store {
	t.Helper()
	return mailstore.New(client, mailstore.NewNotifier("test", nil))
}

func SeedMailbox(t testing.TB, store *mailstore.Store, address string) *ent.Mailbox {
	t.Helper()
	ctx := context.Background()
	mbox, err := store.Client().Mailbox.Create().SetPrimaryAddress(address).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateDefaultFolders(ctx, mbox.ID); err != nil {
		t.Fatal(err)
	}
	return mbox
}

func RawMessage(subject string) []byte {
	return []byte("From: sender@example.com\r\nTo: rcpt@example.com\r\nSubject: " + subject + "\r\nMessage-ID: <" + subject + "@example.com>\r\nDate: Mon, 01 Sep 2025 10:00:00 +0000\r\n\r\nbody of " + subject + "\r\n")
}

func sanitize(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			out = append(out, c)
			continue
		}
		out = append(out, '_')
	}
	return string(out)
}
