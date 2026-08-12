package imapserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	goimapserver "github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"

	passwordauth "github.com/zephyraoss/haitatsu/internal/auth"
	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/apppassword"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/label"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailbox"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessagelabel"
	"github.com/zephyraoss/haitatsu/internal/metrics"
)

const labelPrefix = "Labels/"

type MessageStore interface {
	GetMessage(ctx context.Context, key string) ([]byte, error)
	PutMessage(ctx context.Context, key string, data []byte) error
}

type Server struct {
	addr   string
	server *goimapserver.Server
}

func New(cfg config.IMAPConfig, tlsConfig *tls.Config, client *ent.Client, store MessageStore, metrics *metrics.Metrics) *Server {
	throttle := passwordauth.NewFailureThrottle(10, 10*time.Minute, 15*time.Minute)
	server := goimapserver.New(&goimapserver.Options{
		NewSession: func(conn *goimapserver.Conn) (goimapserver.Session, *goimapserver.GreetingData, error) {
			metrics.IMAPSessionStart()
			return &session{client: client, store: store, metrics: metrics, throttle: throttle, remoteIP: remoteIP(conn.NetConn())}, &goimapserver.GreetingData{}, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIdle: {}, imap.CapUIDPlus: {}, imap.CapMove: {}},
		TLSConfig:    tlsConfig,
		InsecureAuth: tlsConfig == nil,
	})
	return &Server{addr: cfg.Addr, server: server}
}

func (s *Server) Listen() error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	return s.server.Serve(listener)
}

func (s *Server) Shutdown(context.Context) error {
	return s.server.Close()
}

type session struct {
	client       *ent.Client
	store        MessageStore
	metrics      *metrics.Metrics
	throttle     *passwordauth.FailureThrottle
	remoteIP     string
	mailboxID    string
	selectedName string
}

func remoteIP(conn net.Conn) string {
	if conn == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return conn.RemoteAddr().String()
	}
	return host
}

func (s *session) Close() error {
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
	mbox, err := s.client.Mailbox.Query().Where(mailbox.PrimaryAddressEqualFold(username), mailbox.StatusEQ("active"), mailbox.DeletedAtIsNil()).Only(context.Background())
	if err != nil {
		return goimapserver.ErrAuthFailed
	}

	passwords, err := s.client.AppPassword.Query().Where(apppassword.MailboxIDEQ(mbox.ID), apppassword.RevokedAtIsNil(), apppassword.DeletedAtIsNil()).All(context.Background())
	if err != nil {
		return err
	}
	for _, item := range passwords {
		valid, err := passwordauth.VerifyPassword(password, item.Hash)
		if err != nil {
			return err
		}
		if valid && hasScope(item.Scopes, "imap") {
			s.mailboxID = mbox.ID
			_, _ = s.client.AppPassword.UpdateOneID(item.ID).SetLastUsedAt(time.Now()).Save(context.Background())
			return nil
		}
	}
	return goimapserver.ErrAuthFailed
}

func (s *session) Select(mailboxName string, _ *imap.SelectOptions) (*imap.SelectData, error) {
	messages, err := s.messages(mailboxName)
	if err != nil {
		return nil, err
	}
	s.selectedName = mailboxName
	return &imap.SelectData{
		Flags:          []imap.Flag{imap.FlagSeen, imap.FlagFlagged, imap.FlagDeleted},
		PermanentFlags: []imap.Flag{imap.FlagSeen, imap.FlagFlagged, imap.FlagDeleted},
		NumMessages:    uint32(len(messages)),
		UIDNext:        imap.UID(len(messages) + 1),
		UIDValidity:    uidValidity(s.mailboxID),
	}, nil
}

func (s *session) Create(mailboxName string, _ *imap.CreateOptions) error {
	if labelName, ok := strings.CutPrefix(mailboxName, labelPrefix); ok {
		_, err := s.client.Label.Create().SetMailboxID(s.mailboxID).SetName(labelName).Save(context.Background())
		return err
	}
	_, err := s.client.Folder.Create().SetMailboxID(s.mailboxID).SetName(mailboxName).Save(context.Background())
	return err
}

func (s *session) Delete(mailboxName string) error {
	if labelName, ok := strings.CutPrefix(mailboxName, labelPrefix); ok {
		_, err := s.client.Label.Delete().Where(label.MailboxIDEQ(s.mailboxID), label.NameEQ(labelName)).Exec(context.Background())
		return err
	}
	_, err := s.client.Folder.Delete().Where(folder.MailboxIDEQ(s.mailboxID), folder.NameEQ(mailboxName), folder.SystemEQ(false)).Exec(context.Background())
	return err
}

func (s *session) Rename(mailboxName, newName string, _ *imap.RenameOptions) error {
	if strings.HasPrefix(mailboxName, labelPrefix) || strings.HasPrefix(newName, labelPrefix) {
		return unsupported("rename labels")
	}
	_, err := s.client.Folder.Update().Where(folder.MailboxIDEQ(s.mailboxID), folder.NameEQ(mailboxName), folder.SystemEQ(false)).SetName(newName).Save(context.Background())
	return err
}

func (s *session) Subscribe(string) error   { return nil }
func (s *session) Unsubscribe(string) error { return nil }

func (s *session) List(w *goimapserver.ListWriter, _ string, _ []string, _ *imap.ListOptions) error {
	folders, err := s.client.Folder.Query().Where(folder.MailboxIDEQ(s.mailboxID)).All(context.Background())
	if err != nil {
		return err
	}
	for _, item := range folders {
		if err := w.WriteList(&imap.ListData{Mailbox: item.Name, Delim: '/', Attrs: folderAttrs(item.Name)}); err != nil {
			return err
		}
	}
	labels, err := s.client.Label.Query().Where(label.MailboxIDEQ(s.mailboxID)).All(context.Background())
	if err != nil {
		return err
	}
	for _, item := range labels {
		if err := w.WriteList(&imap.ListData{Mailbox: labelPrefix + item.Name, Delim: '/', Attrs: []imap.MailboxAttr{imap.MailboxAttrHasNoChildren}}); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) Status(mailboxName string, _ *imap.StatusOptions) (*imap.StatusData, error) {
	messages, err := s.messages(mailboxName)
	if err != nil {
		return nil, err
	}
	numMessages := uint32(len(messages))
	numUnseen := uint32(0)
	numDeleted := uint32(0)
	for _, item := range messages {
		if !item.Read {
			numUnseen++
		}
		if item.ImapDeleted {
			numDeleted++
		}
	}
	return &imap.StatusData{Mailbox: mailboxName, NumMessages: &numMessages, NumUnseen: &numUnseen, NumDeleted: &numDeleted, UIDNext: imap.UID(len(messages) + 1), UIDValidity: uidValidity(s.mailboxID)}, nil
}

func (s *session) Append(mailboxName string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	return s.appendMessage(mailboxName, r, options)
}

func (s *session) Poll(*goimapserver.UpdateWriter, bool) error { return nil }

func (s *session) Idle(_ *goimapserver.UpdateWriter, stop <-chan struct{}) error {
	<-stop
	return nil
}

func (s *session) Unselect() error {
	s.selectedName = ""
	return nil
}

func (s *session) Expunge(w *goimapserver.ExpungeWriter, _ *imap.UIDSet) error {
	items, err := s.messages(s.selectedName)
	if err != nil {
		return err
	}
	trash, err := s.systemFolder("Trash")
	if err != nil {
		return err
	}
	for index, item := range items {
		if !item.ImapDeleted {
			continue
		}
		if _, err := s.client.MailboxMessage.UpdateOneID(item.ID).SetFolderID(trash.ID).SetImapDeleted(false).Save(context.Background()); err != nil {
			return err
		}
		if err := w.WriteExpunge(uint32(index + 1)); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) Search(kind goimapserver.NumKind, _ *imap.SearchCriteria, _ *imap.SearchOptions) (*imap.SearchData, error) {
	items, err := s.messages(s.selectedName)
	if err != nil {
		return nil, err
	}
	var seqSet imap.SeqSet
	var uidSet imap.UIDSet
	for index := range items {
		num := uint32(index + 1)
		if kind == goimapserver.NumKindUID {
			uidSet.AddNum(imap.UID(num))
		} else {
			seqSet.AddNum(num)
		}
	}
	if kind == goimapserver.NumKindUID {
		return &imap.SearchData{All: uidSet, Count: uint32(len(items))}, nil
	}
	return &imap.SearchData{All: seqSet, Count: uint32(len(items))}, nil
}

func (s *session) Fetch(w *goimapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	items, err := s.messages(s.selectedName)
	if err != nil {
		return err
	}
	for index, item := range items {
		seq := uint32(index + 1)
		if !containsNum(numSet, seq) {
			continue
		}
		if err := s.fetchMessage(w.CreateMessage(seq), item, options, imap.UID(seq)); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) Store(w *goimapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, _ *imap.StoreOptions) error {
	items, err := s.messages(s.selectedName)
	if err != nil {
		return err
	}
	for index, item := range items {
		seq := uint32(index + 1)
		if !containsNum(numSet, seq) {
			continue
		}
		updated, err := s.storeFlags(item, flags)
		if err != nil {
			return err
		}
		if !flags.Silent {
			writer := w.CreateMessage(seq)
			writer.WriteFlags(messageFlags(updated))
			writer.WriteUID(imap.UID(seq))
			if err := writer.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *session) messages(mailboxName string) ([]*ent.MailboxMessage, error) {
	if labelName, ok := strings.CutPrefix(mailboxName, labelPrefix); ok {
		return s.messagesForLabel(labelName)
	}
	folder, err := s.client.Folder.Query().Where(folder.MailboxIDEQ(s.mailboxID), folder.NameEQ(mailboxName)).Only(context.Background())
	if err != nil {
		return nil, err
	}
	return s.client.MailboxMessage.Query().Where(mailboxmessage.MailboxIDEQ(s.mailboxID), mailboxmessage.FolderIDEQ(folder.ID), mailboxmessage.DeletedAtIsNil()).Order(mailboxmessage.ByCreatedAt()).All(context.Background())
}

func (s *session) messagesForLabel(labelName string) ([]*ent.MailboxMessage, error) {
	label, err := s.client.Label.Query().Where(label.MailboxIDEQ(s.mailboxID), label.NameEQ(labelName)).Only(context.Background())
	if err != nil {
		return nil, err
	}
	links, err := s.client.MailboxMessageLabel.Query().Where(mailboxmessagelabel.LabelIDEQ(label.ID)).All(context.Background())
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(links))
	for _, link := range links {
		ids = append(ids, link.MailboxMessageID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return s.client.MailboxMessage.Query().Where(mailboxmessage.IDIn(ids...), mailboxmessage.DeletedAtIsNil()).Order(mailboxmessage.ByCreatedAt()).All(context.Background())
}

func (s *session) fetchMessage(w *goimapserver.FetchResponseWriter, item *ent.MailboxMessage, options *imap.FetchOptions, uid imap.UID) error {
	msg, err := s.client.Message.Get(context.Background(), item.MessageID)
	if err != nil {
		return err
	}
	raw, err := s.store.GetMessage(context.Background(), msg.BlobKey)
	if err != nil {
		return err
	}
	if options.UID {
		w.WriteUID(uid)
	}
	if options.Flags {
		w.WriteFlags(messageFlags(item))
	}
	if options.InternalDate {
		w.WriteInternalDate(item.CreatedAt)
	}
	if options.RFC822Size {
		w.WriteRFC822Size(int64(len(raw)))
	}
	if options.Envelope {
		w.WriteEnvelope(goimapserver.ExtractEnvelope(messageHeader(raw)))
	}
	if options.BodyStructure != nil {
		w.WriteBodyStructure(goimapserver.ExtractBodyStructure(bytes.NewReader(raw)))
	}
	for _, section := range options.BodySection {
		body := goimapserver.ExtractBodySection(bytes.NewReader(raw), section)
		writer := w.WriteBodySection(section, int64(len(body)))
		if _, err := writer.Write(body); err != nil {
			return err
		}
		if err := writer.Close(); err != nil {
			return err
		}
	}
	return w.Close()
}

func (s *session) storeFlags(item *ent.MailboxMessage, flags *imap.StoreFlags) (*ent.MailboxMessage, error) {
	read := item.Read
	flagged := item.Flagged
	deleted := item.ImapDeleted
	apply := func(flag imap.Flag, value bool) {
		switch flag {
		case imap.FlagSeen:
			read = value
		case imap.FlagFlagged:
			flagged = value
		case imap.FlagDeleted:
			deleted = value
		}
	}
	if flags.Op == imap.StoreFlagsSet {
		read, flagged, deleted = false, false, false
	}
	for _, flag := range flags.Flags {
		apply(flag, flags.Op != imap.StoreFlagsDel)
	}
	return s.client.MailboxMessage.UpdateOneID(item.ID).SetRead(read).SetFlagged(flagged).SetImapDeleted(deleted).Save(context.Background())
}

func (s *session) systemFolder(name string) (*ent.Folder, error) {
	return s.client.Folder.Query().Where(folder.MailboxIDEQ(s.mailboxID), folder.NameEQ(name)).Only(context.Background())
}

func hasScope(scopes []string, expected string) bool {
	for _, scope := range scopes {
		if scope == expected {
			return true
		}
	}
	return false
}

func containsNum(numSet imap.NumSet, seq uint32) bool {
	switch set := numSet.(type) {
	case imap.SeqSet:
		return (&set).Contains(seq)
	case imap.UIDSet:
		return set.Contains(imap.UID(seq))
	default:
		return false
	}
}

func messageFlags(item *ent.MailboxMessage) []imap.Flag {
	flags := make([]imap.Flag, 0, 3)
	if item.Read {
		flags = append(flags, imap.FlagSeen)
	}
	if item.Flagged {
		flags = append(flags, imap.FlagFlagged)
	}
	if item.ImapDeleted {
		flags = append(flags, imap.FlagDeleted)
	}
	return flags
}

func folderAttrs(name string) []imap.MailboxAttr {
	switch name {
	case "Archive":
		return []imap.MailboxAttr{imap.MailboxAttrArchive}
	case "Drafts":
		return []imap.MailboxAttr{imap.MailboxAttrDrafts}
	case "Junk":
		return []imap.MailboxAttr{imap.MailboxAttrJunk}
	case "Sent":
		return []imap.MailboxAttr{imap.MailboxAttrSent}
	case "Trash":
		return []imap.MailboxAttr{imap.MailboxAttrTrash}
	default:
		return []imap.MailboxAttr{imap.MailboxAttrHasNoChildren}
	}
}

func uidValidity(mailboxID string) uint32 {
	var value uint32 = 1
	for _, b := range []byte(mailboxID) {
		value = value*31 + uint32(b)
	}
	return value
}

func messageHeader(raw []byte) textproto.Header {
	header, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return textproto.Header{}
	}
	return header
}

func unsupported(command string) error {
	return fmt.Errorf("%s not supported", command)
}
