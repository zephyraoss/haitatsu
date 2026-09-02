package mailstore

import (
	"context"
	"encoding/json"
	"sync"
)

type ChangeKind string

const (
	ChangeExists  ChangeKind = "exists"
	ChangeExpunge ChangeKind = "expunge"
	ChangeFlags   ChangeKind = "flags"
)

type Change struct {
	Node      string     `json:"node"`
	MailboxID string     `json:"mailbox_id"`
	Container string     `json:"container"`
	Kind      ChangeKind `json:"kind"`
	UID       uint32     `json:"uid,omitempty"`
	Flags     []string   `json:"flags,omitempty"`
}

type RemoteBus interface {
	Publish(ctx context.Context, payload []byte) error
	Subscribe(ctx context.Context, handler func(payload []byte)) error
}

type Notifier struct {
	node        string
	mu          sync.RWMutex
	subscribers map[string]map[chan Change]struct{}
	remote      RemoteBus
}

func NewNotifier(node string, remote RemoteBus) *Notifier {
	return &Notifier{node: node, subscribers: map[string]map[chan Change]struct{}{}, remote: remote}
}

func (n *Notifier) Start(ctx context.Context) {
	if n.remote == nil {
		return
	}
	go func() {
		_ = n.remote.Subscribe(ctx, func(payload []byte) {
			var change Change
			if err := json.Unmarshal(payload, &change); err != nil || change.Node == n.node {
				return
			}
			n.dispatch(change)
		})
	}()
}

func (n *Notifier) Subscribe(container string) (<-chan Change, func()) {
	ch := make(chan Change, 256)
	n.mu.Lock()
	if n.subscribers[container] == nil {
		n.subscribers[container] = map[chan Change]struct{}{}
	}
	n.subscribers[container][ch] = struct{}{}
	n.mu.Unlock()
	return ch, func() {
		n.mu.Lock()
		delete(n.subscribers[container], ch)
		if len(n.subscribers[container]) == 0 {
			delete(n.subscribers, container)
		}
		n.mu.Unlock()
	}
}

func (n *Notifier) Publish(ctx context.Context, change Change) {
	if n == nil {
		return
	}
	change.Node = n.node
	n.dispatch(change)
	if n.remote == nil {
		return
	}
	payload, err := json.Marshal(change)
	if err != nil {
		return
	}
	_ = n.remote.Publish(ctx, payload)
}

func (n *Notifier) dispatch(change Change) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	for ch := range n.subscribers[change.Container] {
		select {
		case ch <- change:
		default:
		}
	}
}

func FolderContainer(folderID string) string { return "folder:" + folderID }
func LabelContainer(labelID string) string   { return "label:" + labelID }
