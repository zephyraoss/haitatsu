package imapserver

import (
	"context"
	"slices"
	"time"

	"github.com/emersion/go-imap/v2"
	goimapserver "github.com/emersion/go-imap/v2/imapserver"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessagelabel"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
)

const staleViewInterval = 30 * time.Second

type container struct {
	name        string
	folder      *ent.Folder
	label       *ent.Label
	uidValidity uint32
	uidNext     uint32
	key         string
}

func (c container) isLabel() bool { return c.label != nil }

type entry struct {
	uid       uint32
	itemID    string
	messageID string
	flags     mailstore.Flags
	gone      bool
}

type view struct {
	container     container
	entries       []entry
	lastSync      time.Time
	changes       <-chan mailstore.Change
	cancel        func()
	pendingGone   bool
	pendingExists bool
}

func (v *view) seqOf(uid uint32) uint32 {
	for index, item := range v.entries {
		if item.uid == uid {
			return uint32(index + 1)
		}
	}
	return 0
}

func (v *view) maxUID() uint32 {
	if len(v.entries) == 0 {
		return 0
	}
	return v.entries[len(v.entries)-1].uid
}

func (v *view) selected(numSet imap.NumSet) []int {
	var indexes []int
	switch set := numSet.(type) {
	case imap.SeqSet:
		for index := range v.entries {
			if (&set).Contains(uint32(index + 1)) {
				indexes = append(indexes, index)
			}
		}
		if set.Dynamic() && len(v.entries) > 0 && !slices.Contains(indexes, len(v.entries)-1) {
			indexes = append(indexes, len(v.entries)-1)
		}
	case imap.UIDSet:
		for index, item := range v.entries {
			if set.Contains(imap.UID(item.uid)) {
				indexes = append(indexes, index)
			}
		}
		if set.Dynamic() && len(v.entries) > 0 && !slices.Contains(indexes, len(v.entries)-1) {
			indexes = append(indexes, len(v.entries)-1)
		}
	}
	slices.Sort(indexes)
	return slices.Compact(indexes)
}

func (v *view) drainChanges() bool {
	changed := false
	for {
		select {
		case <-v.changes:
			changed = true
		default:
			return changed
		}
	}
}

func (s *session) loadEntries(ctx context.Context, c container) ([]entry, error) {
	if c.isLabel() {
		return s.loadLabelEntries(ctx, c.label.ID)
	}
	items, err := s.client.MailboxMessage.Query().
		Where(mailboxmessage.FolderIDEQ(c.folder.ID), mailboxmessage.DeletedAtIsNil()).
		Order(mailboxmessage.ByUID()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]entry, 0, len(items))
	for _, item := range items {
		entries = append(entries, entry{uid: item.UID, itemID: item.ID, messageID: item.MessageID, flags: mailstore.FlagsOf(item)})
	}
	return entries, nil
}

func (s *session) loadLabelEntries(ctx context.Context, labelID string) ([]entry, error) {
	links, err := s.client.MailboxMessageLabel.Query().Where(mailboxmessagelabel.LabelIDEQ(labelID)).Order(mailboxmessagelabel.ByUID()).All(ctx)
	if err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(links))
	for _, link := range links {
		ids = append(ids, link.MailboxMessageID)
	}
	items := map[string]*ent.MailboxMessage{}
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		batch, err := s.client.MailboxMessage.Query().Where(mailboxmessage.IDIn(ids[start:end]...), mailboxmessage.DeletedAtIsNil()).All(ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range batch {
			items[item.ID] = item
		}
	}
	entries := make([]entry, 0, len(links))
	for _, link := range links {
		item, ok := items[link.MailboxMessageID]
		if !ok {
			continue
		}
		entries = append(entries, entry{uid: link.UID, itemID: item.ID, messageID: item.MessageID, flags: mailstore.FlagsOf(item)})
	}
	return entries, nil
}

type updateWriter interface {
	WriteExpunge(seqNum uint32) error
	WriteNumMessages(n uint32) error
	WriteMessageFlags(seqNum uint32, uid imap.UID, flags []imap.Flag) error
}

type syncMode struct {
	expunge bool
	exists  bool
	force   bool
}

func (s *session) resync(ctx context.Context, w updateWriter, mode syncMode) error {
	v := s.view
	if v == nil {
		return nil
	}
	changed := v.drainChanges()
	flushGone := mode.expunge && v.pendingGone
	flushExists := mode.exists && v.pendingExists
	if !changed && !mode.force && !flushGone && !flushExists && time.Since(v.lastSync) < staleViewInterval {
		return nil
	}
	fresh, err := s.loadEntries(ctx, v.container)
	if err != nil {
		return err
	}
	v.lastSync = time.Now()
	freshByUID := make(map[uint32]entry, len(fresh))
	for _, item := range fresh {
		freshByUID[item.uid] = item
	}
	known := make(map[uint32]struct{}, len(v.entries))
	next := make([]entry, 0, len(fresh))
	for _, old := range v.entries {
		known[old.uid] = struct{}{}
		current, ok := freshByUID[old.uid]
		switch {
		case ok:
			if w != nil && !flagsEqual(old.flags, current.flags) {
				if err := w.WriteMessageFlags(uint32(len(next)+1), imap.UID(current.uid), imapFlags(current.flags)); err != nil {
					return err
				}
			}
			next = append(next, current)
		case mode.expunge:
			if w != nil {
				if err := w.WriteExpunge(uint32(len(next) + 1)); err != nil {
					return err
				}
			}
		default:
			old.gone = true
			next = append(next, old)
		}
	}
	v.pendingGone = false
	for _, item := range next {
		if item.gone {
			v.pendingGone = true
			break
		}
	}
	appended := false
	v.pendingExists = false
	for _, item := range fresh {
		if _, ok := known[item.uid]; ok {
			continue
		}
		if mode.exists && w != nil {
			next = append(next, item)
			appended = true
			continue
		}
		v.pendingExists = true
	}
	v.entries = next
	if appended {
		return w.WriteNumMessages(uint32(len(next)))
	}
	return nil
}

type expungeOnlyWriter struct {
	write func(seqNum uint32) error
}

func (w expungeOnlyWriter) WriteExpunge(seqNum uint32) error { return w.write(seqNum) }
func (expungeOnlyWriter) WriteNumMessages(uint32) error      { return nil }
func (expungeOnlyWriter) WriteMessageFlags(uint32, imap.UID, []imap.Flag) error {
	return nil
}

func flagsEqual(a, b mailstore.Flags) bool {
	return a.Seen == b.Seen && a.Answered == b.Answered && a.Flagged == b.Flagged && a.Deleted == b.Deleted && a.Draft == b.Draft && slices.Equal(a.Keywords, b.Keywords)
}

func imapFlags(flags mailstore.Flags) []imap.Flag {
	values := flags.List()
	result := make([]imap.Flag, 0, len(values))
	for _, value := range values {
		result = append(result, imap.Flag(value))
	}
	return result
}

func storeFlagStrings(flags []imap.Flag) []string {
	values := make([]string, 0, len(flags))
	for _, flag := range flags {
		values = append(values, string(flag))
	}
	return values
}

var _ updateWriter = (*goimapserver.UpdateWriter)(nil)
