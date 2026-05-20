package bounce

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/ids"
)

type Store interface {
	PutMessage(ctx context.Context, key string, data []byte) error
}

type Handler struct {
	client *ent.Client
	store  Store
	domain string
}

type Recipient struct {
	Address   string
	MessageID string
}

func NewHandler(client *ent.Client, store Store, domain string) *Handler {
	return &Handler{client: client, store: store, domain: strings.ToLower(domain)}
}

func (h *Handler) ParseRecipient(address string) (Recipient, bool, bool) {
	local, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(address)), "@")
	if !ok || domain != h.domain {
		return Recipient{}, false, false
	}
	messageID, ok := strings.CutPrefix(local, "bounce+")
	if !ok {
		return Recipient{}, true, false
	}
	if _, err := ulid.Parse(messageID); err != nil {
		return Recipient{}, true, false
	}
	return Recipient{Address: address, MessageID: messageID}, true, true
}

func (h *Handler) Record(ctx context.Context, recipient Recipient, raw []byte) error {
	key := fmt.Sprintf("bounces/%s/%s.eml", recipient.MessageID, ids.New().String())
	if err := h.store.PutMessage(ctx, key, raw); err != nil {
		return err
	}
	_, err := h.client.BounceEvent.Create().
		SetMessageID(recipient.MessageID).
		SetRecipient(recipient.Address).
		SetBlobKey(key).
		SetSha256(sha256Hex(raw)).
		SetSizeBytes(int64(len(raw))).
		SetDetails(map[string]any{"received_at": time.Now().UTC().Format(time.RFC3339)}).
		Save(ctx)
	return err
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
