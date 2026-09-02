package imapserver

import (
	"context"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	goimapserver "github.com/emersion/go-imap/v2/imapserver"

	passwordauth "github.com/zephyraoss/haitatsu/internal/auth"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/apppassword"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/label"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailbox"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessagelabel"
	"github.com/zephyraoss/haitatsu/internal/database/ent/message"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/ratelimit"
)

var permanentFlags = []imap.Flag{imap.FlagSeen, imap.FlagAnswered, imap.FlagFlagged, imap.FlagDeleted, imap.FlagDraft, imap.FlagWildcard}

type session struct {
	client      *ent.Client
	blobs       MessageStore
	store       *mailstore.Store
	metrics     *metrics.Metrics
	throttle    *passwordauth.FailureThrottle
	gate        *ratelimit.ConcurrencyGate
	remoteIP    string
	appendLimit int64
	mailboxID   string
	view        *view
	readOnly    bool
}

func (s *session) Close() error {
	s.closeView()
	s.gate.Release(s.remoteIP)
	s.metrics.IMAPSessionEnd()
	return nil
}

func (s *session) Login(username, password string) error {
	if s.throttle.Blocked(s.remoteIP) {
		return goimapserver.ErrAuthFailed
	}
	if err := s.login(username, password); err != nil {
		s.throttle.RecordFailure(s.remoteIP)
		return err
	}
	s.throttle.RecordSuccess(s.remoteIP)
	return nil
}

func (s *session) login(username, password string) error {
	ctx := context.Background()
	mbox, err := s.client.Mailbox.Query().Where(mailbox.PrimaryAddressEqualFold(strings.TrimSpace(username)), mailbox.StatusEQ("active"), mailbox.DeletedAtIsNil()).Only(ctx)
	if err != nil {
		return goimapserver.ErrAuthFailed
	}
	passwords, err := s.client.AppPassword.Query().Where(apppassword.MailboxIDEQ(mbox.ID), apppassword.RevokedAtIsNil(), apppassword.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return err
	}
	for _, item := range passwords {
		valid, err := passwordauth.VerifyPassword(password, item.Hash)
		if err != nil {
			return err
		}
		if valid && passwordauth.HasScope(item.Scopes, "imap") {
			s.mailboxID = mbox.ID
			_, _ = s.client.AppPassword.UpdateOneID(item.ID).SetLastUsedAt(time.Now()).Save(ctx)
			return nil
		}
	}
	return goimapserver.ErrAuthFailed
}

func (s *session) Namespace() (*imap.NamespaceData, error) {
	return &imap.NamespaceData{Personal: []imap.NamespaceDescriptor{{Prefix: "", Delim: '/'}}}, nil
}

func (s *session) resolve(ctx context.Context, name string) (container, error) {
	if labelName, ok := strings.CutPrefix(name, labelPrefix); ok {
		item, err := s.client.Label.Query().Where(label.MailboxIDEQ(s.mailboxID), label.NameEQ(labelName)).Only(ctx)
		if err != nil {
			return container{}, notFound(err)
		}
		return container{name: name, label: item, uidValidity: item.UIDValidity, uidNext: item.UIDNext, key: mailstore.LabelContainer(item.ID)}, nil
	}
	if strings.EqualFold(name, "INBOX") {
		name = "INBOX"
	}
	item, err := s.client.Folder.Query().Where(folder.MailboxIDEQ(s.mailboxID), folder.NameEQ(name)).Only(ctx)
	if err != nil {
		return container{}, notFound(err)
	}
	return container{name: name, folder: item, uidValidity: item.UIDValidity, uidNext: item.UIDNext, key: mailstore.FolderContainer(item.ID)}, nil
}

func notFound(err error) error {
	if ent.IsNotFound(err) {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "Mailbox does not exist"}
	}
	return err
}

func (s *session) Select(mailboxName string, options *imap.SelectOptions) (*imap.SelectData, error) {
	ctx := context.Background()
	c, err := s.resolve(ctx, mailboxName)
	if err != nil {
		return nil, err
	}
	entries, err := s.loadEntries(ctx, c)
	if err != nil {
		return nil, err
	}
	s.closeView()
	changes, cancel := s.store.Notifier().Subscribe(c.key)
	s.view = &view{container: c, entries: entries, lastSync: time.Now(), changes: changes, cancel: cancel}
	s.readOnly = options != nil && options.ReadOnly
	var firstUnseen uint32
	for index, item := range entries {
		if !item.flags.Seen {
			firstUnseen = uint32(index + 1)
			break
		}
	}
	return &imap.SelectData{
		Flags:             permanentFlags[:len(permanentFlags)-1],
		PermanentFlags:    permanentFlags,
		NumMessages:       uint32(len(entries)),
		FirstUnseenSeqNum: firstUnseen,
		UIDNext:           imap.UID(c.uidNext),
		UIDValidity:       c.uidValidity,
	}, nil
}

func (s *session) closeView() {
	if s.view != nil && s.view.cancel != nil {
		s.view.cancel()
	}
	s.view = nil
}

func (s *session) Unselect() error {
	s.closeView()
	return nil
}

func (s *session) Create(mailboxName string, _ *imap.CreateOptions) error {
	ctx := context.Background()
	mailboxName = strings.TrimSuffix(mailboxName, "/")
	if labelName, ok := strings.CutPrefix(mailboxName, labelPrefix); ok {
		_, err := s.store.CreateLabel(ctx, s.mailboxID, labelName)
		return alreadyExists(err)
	}
	if strings.EqualFold(mailboxName, "INBOX") || strings.EqualFold(mailboxName, strings.TrimSuffix(labelPrefix, "/")) {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlreadyExists, Text: "Mailbox already exists"}
	}
	_, err := s.store.CreateFolder(ctx, s.mailboxID, mailboxName, false)
	return alreadyExists(err)
}

func alreadyExists(err error) error {
	if ent.IsConstraintError(err) {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlreadyExists, Text: "Mailbox already exists"}
	}
	return err
}

func (s *session) Delete(mailboxName string) error {
	ctx := context.Background()
	c, err := s.resolve(ctx, mailboxName)
	if err != nil {
		return err
	}
	if c.isLabel() {
		_, err := s.client.MailboxMessageLabel.Delete().Where(mailboxmessagelabel.LabelIDEQ(c.label.ID)).Exec(ctx)
		if err != nil {
			return err
		}
		return s.client.Label.DeleteOneID(c.label.ID).Exec(ctx)
	}
	if c.folder.System {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeCannot, Text: "System folders cannot be deleted"}
	}
	children, err := s.client.Folder.Query().Where(folder.MailboxIDEQ(s.mailboxID), folder.NameHasPrefix(c.folder.Name+"/")).Count(ctx)
	if err != nil {
		return err
	}
	if children > 0 {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeHasChildren, Text: "Mailbox has child mailboxes"}
	}
	items, err := s.store.ActiveMessagesInFolder(ctx, c.folder.ID)
	if err != nil {
		return err
	}
	if err := s.store.SoftDeleteMany(ctx, items); err != nil {
		return err
	}
	return s.client.Folder.DeleteOneID(c.folder.ID).Exec(ctx)
}

func (s *session) Rename(mailboxName, newName string, _ *imap.RenameOptions) error {
	ctx := context.Background()
	if strings.HasPrefix(mailboxName, labelPrefix) != strings.HasPrefix(newName, labelPrefix) {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeCannot, Text: "Cannot rename between folders and labels"}
	}
	c, err := s.resolve(ctx, mailboxName)
	if err != nil {
		return err
	}
	if c.isLabel() {
		_, err := s.client.Label.UpdateOneID(c.label.ID).SetName(strings.TrimPrefix(newName, labelPrefix)).Save(ctx)
		return alreadyExists(err)
	}
	if c.folder.System {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeCannot, Text: "System folders cannot be renamed"}
	}
	children, err := s.client.Folder.Query().Where(folder.MailboxIDEQ(s.mailboxID), folder.NameHasPrefix(c.folder.Name+"/")).All(ctx)
	if err != nil {
		return err
	}
	if _, err := s.client.Folder.UpdateOneID(c.folder.ID).SetName(newName).Save(ctx); err != nil {
		return alreadyExists(err)
	}
	for _, child := range children {
		renamed := newName + strings.TrimPrefix(child.Name, c.folder.Name)
		if _, err := s.client.Folder.UpdateOneID(child.ID).SetName(renamed).Save(ctx); err != nil {
			return alreadyExists(err)
		}
	}
	return nil
}

func (s *session) Subscribe(string) error   { return nil }
func (s *session) Unsubscribe(string) error { return nil }

func (s *session) List(w *goimapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	ctx := context.Background()
	folders, err := s.client.Folder.Query().Where(folder.MailboxIDEQ(s.mailboxID)).Order(folder.ByName()).All(ctx)
	if err != nil {
		return err
	}
	labels, err := s.client.Label.Query().Where(label.MailboxIDEQ(s.mailboxID)).Order(label.ByName()).All(ctx)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(folders)+len(labels)+1)
	for _, item := range folders {
		names = append(names, item.Name)
	}
	if len(labels) > 0 {
		names = append(names, strings.TrimSuffix(labelPrefix, "/"))
	}
	for _, item := range labels {
		names = append(names, labelPrefix+item.Name)
	}
	if len(patterns) == 0 {
		return w.WriteList(&imap.ListData{Delim: '/', Mailbox: "", Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}})
	}
	for _, name := range names {
		if !matchesAny(name, ref, patterns) {
			continue
		}
		data := &imap.ListData{Mailbox: name, Delim: '/', Attrs: s.listAttrs(name, names)}
		if options != nil && options.ReturnStatus != nil && !isLabelRoot(name) {
			status, err := s.Status(name, options.ReturnStatus)
			if err == nil {
				data.Status = status
			}
		}
		if err := w.WriteList(data); err != nil {
			return err
		}
	}
	return nil
}

func isLabelRoot(name string) bool {
	return name == strings.TrimSuffix(labelPrefix, "/")
}

func matchesAny(name string, ref string, patterns []string) bool {
	for _, pattern := range patterns {
		if goimapserver.MatchList(name, '/', ref, pattern) {
			return true
		}
	}
	return false
}

func (s *session) listAttrs(name string, all []string) []imap.MailboxAttr {
	attrs := []imap.MailboxAttr{}
	if isLabelRoot(name) {
		attrs = append(attrs, imap.MailboxAttrNoSelect)
	}
	switch name {
	case "Archive":
		attrs = append(attrs, imap.MailboxAttrArchive)
	case "Drafts":
		attrs = append(attrs, imap.MailboxAttrDrafts)
	case "Junk":
		attrs = append(attrs, imap.MailboxAttrJunk)
	case "Sent":
		attrs = append(attrs, imap.MailboxAttrSent)
	case "Trash":
		attrs = append(attrs, imap.MailboxAttrTrash)
	}
	hasChildren := false
	for _, other := range all {
		if strings.HasPrefix(other, name+"/") {
			hasChildren = true
			break
		}
	}
	if hasChildren {
		return append(attrs, imap.MailboxAttrHasChildren)
	}
	return append(attrs, imap.MailboxAttrHasNoChildren)
}

func (s *session) Status(mailboxName string, options *imap.StatusOptions) (*imap.StatusData, error) {
	ctx := context.Background()
	c, err := s.resolve(ctx, mailboxName)
	if err != nil {
		return nil, err
	}
	data := &imap.StatusData{Mailbox: mailboxName, UIDNext: imap.UID(c.uidNext), UIDValidity: c.uidValidity}
	if options == nil {
		return data, nil
	}
	if options.NumMessages || options.NumUnseen || options.NumDeleted || options.Size {
		entries, err := s.loadEntries(ctx, c)
		if err != nil {
			return nil, err
		}
		total := uint32(len(entries))
		var unseen, deleted uint32
		for _, item := range entries {
			if !item.flags.Seen {
				unseen++
			}
			if item.flags.Deleted {
				deleted++
			}
		}
		if options.NumMessages {
			data.NumMessages = &total
		}
		if options.NumUnseen {
			data.NumUnseen = &unseen
		}
		if options.NumDeleted {
			data.NumDeleted = &deleted
		}
		if options.Size {
			size, err := s.sizeOf(ctx, entries)
			if err != nil {
				return nil, err
			}
			data.Size = &size
		}
	}
	if options.AppendLimit && s.appendLimit > 0 {
		limit := uint32(min(s.appendLimit, int64(^uint32(0))))
		data.AppendLimit = &limit
	}
	return data, nil
}

func (s *session) sizeOf(ctx context.Context, entries []entry) (int64, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(entries))
	for _, item := range entries {
		ids = append(ids, item.messageID)
	}
	var total int64
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		msgs, err := s.client.Message.Query().Where(message.IDIn(ids[start:end]...)).Select(message.FieldSizeBytes).All(ctx)
		if err != nil {
			return 0, err
		}
		for _, msg := range msgs {
			total += msg.SizeBytes
		}
	}
	return total, nil
}

func (s *session) Poll(w *goimapserver.UpdateWriter, allowExpunge bool) error {
	if s.view == nil {
		return nil
	}
	return s.resync(context.Background(), w, syncMode{expunge: allowExpunge, exists: true})
}

func (s *session) Idle(w *goimapserver.UpdateWriter, stop <-chan struct{}) error {
	if s.view == nil {
		<-stop
		return nil
	}
	ticker := time.NewTicker(staleViewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-s.view.changes:
			if err := s.resync(context.Background(), w, syncMode{expunge: true, exists: true, force: true}); err != nil {
				return err
			}
		case <-ticker.C:
			if err := s.resync(context.Background(), w, syncMode{expunge: true, exists: true, force: true}); err != nil {
				return err
			}
		}
	}
}

func (s *session) Expunge(w *goimapserver.ExpungeWriter, uids *imap.UIDSet) error {
	if s.view == nil {
		return errNotSelected
	}
	if s.readOnly {
		return errReadOnly
	}
	ctx := context.Background()
	if err := s.resync(ctx, expungeOnlyWriter{write: w.WriteExpunge}, syncMode{expunge: true, force: true}); err != nil {
		return err
	}
	remaining := make([]entry, 0, len(s.view.entries))
	for _, item := range s.view.entries {
		if !item.flags.Deleted || (uids != nil && !uids.Contains(imap.UID(item.uid))) {
			remaining = append(remaining, item)
			continue
		}
		if err := s.expungeEntry(ctx, item); err != nil {
			return err
		}
		if err := w.WriteExpunge(uint32(len(remaining) + 1)); err != nil {
			return err
		}
	}
	s.view.entries = remaining
	s.view.drainChanges()
	return nil
}

func (s *session) expungeEntry(ctx context.Context, item entry) error {
	if s.view.container.isLabel() {
		mm, err := s.client.MailboxMessage.Get(ctx, item.itemID)
		if err != nil {
			return err
		}
		return s.store.RemoveLabel(ctx, mm, s.view.container.label.ID)
	}
	mm, err := s.client.MailboxMessage.Get(ctx, item.itemID)
	if err != nil {
		return err
	}
	if s.view.container.folder.Name == "Trash" {
		return s.store.SoftDelete(ctx, mm)
	}
	_, err = s.store.MoveToFolderName(ctx, mm, "Trash")
	return err
}

func (s *session) Store(w *goimapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, _ *imap.StoreOptions) error {
	if s.view == nil {
		return errNotSelected
	}
	if s.readOnly {
		return errReadOnly
	}
	ctx := context.Background()
	if err := s.resync(ctx, nil, syncMode{}); err != nil {
		return err
	}
	for _, index := range s.view.selected(numSet) {
		item := s.view.entries[index]
		if item.gone {
			continue
		}
		next := item.flags
		switch flags.Op {
		case imap.StoreFlagsSet:
			next = mailstore.ParseFlags(storeFlagStrings(flags.Flags))
		case imap.StoreFlagsAdd:
			for _, flag := range flags.Flags {
				next.Set(string(flag), true)
			}
		case imap.StoreFlagsDel:
			for _, flag := range flags.Flags {
				next.Set(string(flag), false)
			}
		}
		mm, err := s.client.MailboxMessage.Get(ctx, item.itemID)
		if err != nil {
			if ent.IsNotFound(err) {
				continue
			}
			return err
		}
		if _, err := s.store.SetFlags(ctx, mm, next); err != nil {
			return err
		}
		s.view.entries[index].flags = next
		if !flags.Silent {
			writer := w.CreateMessage(uint32(index + 1))
			writer.WriteFlags(imapFlags(next))
			writer.WriteUID(imap.UID(item.uid))
			if err := writer.Close(); err != nil {
				return err
			}
		}
	}
	s.view.drainChanges()
	return nil
}

var errNotSelected = &imap.Error{Type: imap.StatusResponseTypeBad, Text: "No mailbox selected"}
var errReadOnly = &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNoPerm, Text: "Mailbox is read-only"}

func (s *session) mailboxMessage(ctx context.Context, item entry) (*ent.MailboxMessage, error) {
	mm, err := s.client.MailboxMessage.Query().Where(mailboxmessage.IDEQ(item.itemID), mailboxmessage.DeletedAtIsNil()).Only(ctx)
	if err != nil {
		return nil, err
	}
	return mm, nil
}
