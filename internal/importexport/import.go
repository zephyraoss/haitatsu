package importexport

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailbox"
	messagepred "github.com/zephyraoss/haitatsu/internal/database/ent/message"
	"github.com/zephyraoss/haitatsu/internal/events"
	"github.com/zephyraoss/haitatsu/internal/ids"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
)

type ImportWorker struct {
	db       *sql.DB
	client   *ent.Client
	store    Store
	events   *events.Service
	workerID string
}

type importJob struct {
	ID         string
	MailboxID  string
	SourceType string
	Source     map[string]any
}

func NewImportWorker(db *sql.DB, client *ent.Client, store Store, events *events.Service, workerID string) *ImportWorker {
	return &ImportWorker{db: db, client: client, store: store, events: events, workerID: workerID}
}

func (w *ImportWorker) Run(ctx context.Context, concurrency int) {
	if concurrency <= 0 {
		concurrency = 1
	}
	for range concurrency {
		go w.loop(ctx)
	}
}

func (w *ImportWorker) loop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = w.processOne(ctx)
		}
	}
}

func (w *ImportWorker) processOne(ctx context.Context) error {
	job, ok, err := w.claim(ctx)
	if err != nil || !ok {
		return err
	}
	count, err := w.importJob(ctx, job)
	if err != nil {
		return w.fail(ctx, job, err)
	}
	_, err = w.client.ImportJob.UpdateOneID(job.ID).SetStatus("completed").SetImportedCount(count).ClearLockedBy().ClearLockedUntil().Save(ctx)
	if err != nil {
		return err
	}
	return w.events.Emit(ctx, events.MailboxImportCompleted, job.MailboxID, map[string]any{"event": events.MailboxImportCompleted, "import_id": job.ID, "mailbox_id": job.MailboxID, "imported_count": count})
}

func (w *ImportWorker) claim(ctx context.Context) (importJob, bool, error) {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return importJob{}, false, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `
UPDATE import_jobs SET locked_by = $1, locked_until = $2, status = 'processing', updated_at = NOW()
WHERE id = (
  SELECT id FROM import_jobs
  WHERE status = 'queued' AND (locked_until IS NULL OR locked_until <= NOW())
  ORDER BY created_at
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING id, mailbox_id, source_type, source
`, w.workerID, time.Now().Add(10*time.Minute))

	var job importJob
	var source []byte
	if err := row.Scan(&job.ID, &job.MailboxID, &job.SourceType, &source); err != nil {
		if err == sql.ErrNoRows {
			return importJob{}, false, nil
		}
		return importJob{}, false, err
	}
	if err := json.Unmarshal(source, &job.Source); err != nil {
		return importJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return importJob{}, false, err
	}
	return job, true, nil
}

func (w *ImportWorker) importJob(ctx context.Context, job importJob) (int, error) {
	mbox, err := w.client.Mailbox.Query().Where(mailbox.IDEQ(job.MailboxID)).Only(ctx)
	if err != nil {
		return 0, err
	}
	inbox, err := w.client.Folder.Query().Where(folder.MailboxIDEQ(job.MailboxID), folder.NameEQ("INBOX")).Only(ctx)
	if err != nil {
		return 0, err
	}
	folders := newFolderCache(inbox.ID)
	switch job.SourceType {
	case "zip":
		return w.importZip(ctx, job, mbox, inbox.ID)
	case "maildir":
		return w.importMaildir(ctx, job, mbox, folders)
	case "imap":
		return w.importIMAP(ctx, job, mbox, folders)
	default:
		return 0, fmt.Errorf("unsupported import source type %q", job.SourceType)
	}
}

// importFlags carries per-message state recovered from the source. Maildir
// answered/draft flags have no MailboxMessage counterpart and are dropped.
type importFlags struct {
	read    bool
	flagged bool
	deleted bool
}

type folderCache struct {
	ids map[string]string
}

func newFolderCache(inboxID string) *folderCache {
	return &folderCache{ids: map[string]string{"INBOX": inboxID}}
}

func (w *ImportWorker) ensureFolder(ctx context.Context, cache *folderCache, mailboxID string, name string) (string, error) {
	if id, ok := cache.ids[name]; ok {
		return id, nil
	}
	existing, err := w.client.Folder.Query().Where(folder.MailboxIDEQ(mailboxID), folder.NameEQ(name)).First(ctx)
	if err == nil {
		cache.ids[name] = existing.ID
		return existing.ID, nil
	}
	if !ent.IsNotFound(err) {
		return "", err
	}
	created, err := w.client.Folder.Create().SetMailboxID(mailboxID).SetName(name).Save(ctx)
	if err == nil {
		cache.ids[name] = created.ID
		return created.ID, nil
	}
	if !ent.IsConstraintError(err) {
		return "", err
	}
	existing, err = w.client.Folder.Query().Where(folder.MailboxIDEQ(mailboxID), folder.NameEQ(name)).First(ctx)
	if err != nil {
		return "", err
	}
	cache.ids[name] = existing.ID
	return existing.ID, nil
}

// systemFolderAliases maps common provider folder names onto the system
// folders created with every mailbox, so imports reuse them instead of
// creating near-duplicates like "Sent Items" next to "Sent".
var systemFolderAliases = map[string]string{
	"inbox":            "INBOX",
	"sent":             "Sent",
	"sent items":       "Sent",
	"sent messages":    "Sent",
	"sent mail":        "Sent",
	"drafts":           "Drafts",
	"draft":            "Drafts",
	"trash":            "Trash",
	"deleted items":    "Trash",
	"deleted messages": "Trash",
	"junk":             "Junk",
	"junk e-mail":      "Junk",
	"junk email":       "Junk",
	"spam":             "Junk",
	"bulk mail":        "Junk",
	"archive":          "Archive",
	"archives":         "Archive",
}

func canonicalFolderSegment(segment string) string {
	if mapped, ok := systemFolderAliases[strings.ToLower(segment)]; ok {
		return mapped
	}
	return segment
}

func (w *ImportWorker) importZip(ctx context.Context, job importJob, mbox *ent.Mailbox, inboxID string) (int, error) {
	key := sourceKey(job.Source)
	if key == "" {
		return 0, fmt.Errorf("zip import requires source.object_key")
	}
	data, err := w.store.GetObject(ctx, key)
	if err != nil {
		return 0, err
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0, err
	}

	imported := 0
	for _, file := range archive.File {
		if file.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(file.Name), ".eml") {
			continue
		}
		raw, err := readZipFile(file)
		if err != nil {
			return imported, err
		}
		msg, err := w.messageForRaw(ctx, raw)
		if err != nil {
			return imported, err
		}
		created, err := w.attachMessage(ctx, job.MailboxID, inboxID, mbox.PrimaryAddress, msg.ID, int64(len(raw)), importFlags{})
		if err != nil {
			return imported, err
		}
		if created {
			imported++
		}
	}
	return imported, nil
}

func (w *ImportWorker) importMaildir(ctx context.Context, job importJob, mbox *ent.Mailbox, folders *folderCache) (int, error) {
	root := strings.TrimSpace(sourceString(job.Source, "path"))
	if root == "" {
		return 0, fmt.Errorf("maildir import requires source.path")
	}
	entries, err := maildirEntries(root)
	if err != nil {
		return 0, err
	}
	imported := 0
	for _, entry := range entries {
		raw, err := os.ReadFile(entry.path)
		if err != nil {
			return imported, err
		}
		folderID, err := w.ensureFolder(ctx, folders, job.MailboxID, entry.folder)
		if err != nil {
			return imported, err
		}
		msg, err := w.messageForRaw(ctx, raw)
		if err != nil {
			return imported, err
		}
		created, err := w.attachMessage(ctx, job.MailboxID, folderID, mbox.PrimaryAddress, msg.ID, int64(len(raw)), entry.flags)
		if err != nil {
			return imported, err
		}
		if created {
			imported++
		}
	}
	return imported, nil
}

func (w *ImportWorker) importIMAP(ctx context.Context, job importJob, mbox *ent.Mailbox, folders *folderCache) (int, error) {
	client, err := dialIMAP(job.Source)
	if err != nil {
		return 0, err
	}
	defer client.Close()
	if err := client.Login(sourceString(job.Source, "username"), sourceString(job.Source, "password")).Wait(); err != nil {
		return 0, err
	}
	mailboxes := sourceStringSlice(job.Source, "mailboxes")
	if len(mailboxes) == 0 {
		mailboxName := sourceString(job.Source, "mailbox")
		if mailboxName == "" {
			mailboxName = "INBOX"
		}
		mailboxes = []string{mailboxName}
	}
	delim := remoteDelimiter(client)
	imported := 0
	for _, mailboxName := range mailboxes {
		folderID, err := w.ensureFolder(ctx, folders, job.MailboxID, destinationFolderName(mailboxName, delim))
		if err != nil {
			return imported, err
		}
		count, err := w.importIMAPMailbox(ctx, client, mailboxName, job.MailboxID, folderID, mbox.PrimaryAddress)
		if err != nil {
			return imported, err
		}
		imported += count
	}
	return imported, nil
}

// remoteDelimiter asks the remote server for its hierarchy delimiter via a
// LIST "" "" query, falling back to "/" if the server gives nothing usable.
func remoteDelimiter(client *imapclient.Client) string {
	items, err := client.List("", "", nil).Collect()
	if err != nil || len(items) == 0 || items[0].Delim == 0 {
		return "/"
	}
	return string(items[0].Delim)
}

// destinationFolderName translates a remote mailbox name into a local folder
// path: the remote delimiter becomes "/", a leading INBOX namespace prefix
// (Courier-style "INBOX.Sent") is dropped, and well-known names map onto
// system folders.
func destinationFolderName(remoteName string, delim string) string {
	name := remoteName
	if delim != "" && delim != "/" {
		name = strings.ReplaceAll(name, delim, "/")
	}
	segments := strings.Split(name, "/")
	if len(segments) > 1 && strings.EqualFold(segments[0], "INBOX") {
		segments = segments[1:]
	}
	segments[0] = canonicalFolderSegment(segments[0])
	return strings.Join(segments, "/")
}

func (w *ImportWorker) importIMAPMailbox(ctx context.Context, client *imapclient.Client, mailboxName string, mailboxID string, folderID string, primaryAddress string) (int, error) {
	selected, err := client.Select(mailboxName, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return 0, err
	}
	if selected.NumMessages == 0 {
		return 0, nil
	}
	bodySection := &imap.FetchItemBodySection{}
	var seqSet imap.SeqSet
	seqSet.AddRange(1, selected.NumMessages)
	fetch := client.Fetch(seqSet, &imap.FetchOptions{Flags: true, BodySection: []*imap.FetchItemBodySection{bodySection}})
	imported := 0
	closeWithError := func(err error) (int, error) {
		_ = fetch.Close()
		return imported, err
	}
	for {
		remoteMessage := fetch.Next()
		if remoteMessage == nil {
			break
		}
		raw, flags, err := fetchMessageData(remoteMessage)
		if err != nil {
			return closeWithError(err)
		}
		if len(raw) == 0 {
			continue
		}
		msg, err := w.messageForRaw(ctx, raw)
		if err != nil {
			return closeWithError(err)
		}
		created, err := w.attachMessage(ctx, mailboxID, folderID, primaryAddress, msg.ID, int64(len(raw)), flags)
		if err != nil {
			return closeWithError(err)
		}
		if created {
			imported++
		}
	}
	if err := fetch.Close(); err != nil {
		return imported, err
	}
	return imported, nil
}

func (w *ImportWorker) attachMessage(ctx context.Context, mailboxID string, folderID string, primaryAddress string, messageID string, size int64, flags importFlags) (bool, error) {
	_, err := w.client.MailboxMessage.Create().
		SetMailboxID(mailboxID).
		SetMessageID(messageID).
		SetFolderID(folderID).
		SetOriginalRcpt(primaryAddress).
		SetBaseRcpt(primaryAddress).
		SetRead(flags.read).
		SetFlagged(flags.flagged).
		SetImapDeleted(flags.deleted).
		Save(ctx)
	if ent.IsConstraintError(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := w.client.Mailbox.UpdateOneID(mailboxID).AddUsedBytes(size).Save(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (w *ImportWorker) messageForRaw(ctx context.Context, raw []byte) (*ent.Message, error) {
	sha := sha256Hex(raw)
	existing, err := w.client.Message.Query().Where(messagepred.Sha256EQ(sha)).First(ctx)
	if err == nil {
		return existing, nil
	}
	if err != nil && !ent.IsNotFound(err) {
		return nil, err
	}

	messageID := ids.New().String()
	traceID := ids.New().String()
	key := messageObjectKey(time.Now().UTC(), messageID)
	if err := w.store.PutMessage(ctx, key, raw); err != nil {
		return nil, err
	}
	metadata := mailparse.Parse(raw)
	create := w.client.Message.Create().
		SetID(messageID).
		SetTraceID(traceID).
		SetBlobKey(key).
		SetSha256(sha).
		SetSizeBytes(int64(len(raw))).
		SetHeaders(metadata.Headers).
		SetFromAddresses(metadata.From).
		SetToAddresses(metadata.To).
		SetCcAddresses(metadata.CC).
		SetBccAddresses(metadata.BCC).
		SetSubject(metadata.Subject).
		SetTextBodyExtract(metadata.TextExtract).
		SetHTMLBodyExtract(metadata.HTMLExtract).
		SetAttachments(metadata.Attachments).
		SetAuthResults(map[string]any{})
	if metadata.RFCMessageID != "" {
		create.SetRfcMessageID(metadata.RFCMessageID)
	}
	if metadata.Date != nil {
		create.SetDate(*metadata.Date)
	}
	return create.Save(ctx)
}

func (w *ImportWorker) fail(ctx context.Context, job importJob, cause error) error {
	_, err := w.client.ImportJob.UpdateOneID(job.ID).SetStatus("failed").SetLastError(map[string]any{"error": cause.Error()}).ClearLockedBy().ClearLockedUntil().Save(ctx)
	if err != nil {
		return err
	}
	return w.events.Emit(ctx, events.MailboxImportFailed, job.MailboxID, map[string]any{"event": events.MailboxImportFailed, "import_id": job.ID, "mailbox_id": job.MailboxID, "error": cause.Error()})
}

func sourceKey(source map[string]any) string {
	if value, _ := source["object_key"].(string); value != "" {
		return value
	}
	value, _ := source["key"].(string)
	return value
}

func sourceString(source map[string]any, key string) string {
	value, _ := source[key].(string)
	return strings.TrimSpace(value)
}

func sourceBool(source map[string]any, key string) bool {
	value, _ := source[key].(bool)
	return value
}

func sourceStringSlice(source map[string]any, key string) []string {
	value, ok := source[key]
	if !ok {
		return nil
	}
	if stringsValue, ok := value.([]string); ok {
		return stringsValue
	}
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		if value, ok := item.(string); ok && strings.TrimSpace(value) != "" {
			values = append(values, strings.TrimSpace(value))
		}
	}
	return values
}

func dialIMAP(source map[string]any) (*imapclient.Client, error) {
	addr := sourceString(source, "addr")
	if addr == "" {
		return nil, fmt.Errorf("imap import requires source.addr")
	}
	options := &imapclient.Options{TLSConfig: imapTLSConfig(addr, sourceBool(source, "skip_verify"))}
	if sourceBool(source, "starttls") {
		return imapclient.DialStartTLS(addr, options)
	}
	if tlsEnabled, ok := source["tls"].(bool); ok && !tlsEnabled {
		return imapclient.DialInsecure(addr, options)
	}
	return imapclient.DialTLS(addr, options)
}

func imapTLSConfig(addr string, skipVerify bool) *tls.Config {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return &tls.Config{ServerName: host, InsecureSkipVerify: skipVerify}
}

func fetchMessageData(message *imapclient.FetchMessageData) ([]byte, importFlags, error) {
	var raw []byte
	var flags importFlags
	for {
		item := message.Next()
		if item == nil {
			return raw, flags, nil
		}
		switch data := item.(type) {
		case imapclient.FetchItemDataBodySection:
			if data.Literal == nil {
				continue
			}
			body, err := io.ReadAll(data.Literal)
			if err != nil {
				return nil, flags, err
			}
			raw = body
		case imapclient.FetchItemDataFlags:
			for _, flag := range data.Flags {
				switch flag {
				case imap.FlagSeen:
					flags.read = true
				case imap.FlagFlagged:
					flags.flagged = true
				case imap.FlagDeleted:
					flags.deleted = true
				}
			}
		}
	}
}

type maildirEntry struct {
	path   string
	folder string
	flags  importFlags
}

func maildirEntries(root string) ([]maildirEntry, error) {
	var entries []maildirEntry
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "tmp" {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		parent := filepath.Base(filepath.Dir(path))
		if parent != "cur" && parent != "new" {
			return nil
		}
		entries = append(entries, maildirEntry{
			path:   path,
			folder: maildirFolderName(root, filepath.Dir(filepath.Dir(path))),
			flags:  maildirFlags(entry.Name()),
		})
		return nil
	})
	return entries, err
}

// maildirFolderName maps the directory holding cur/new onto a local folder
// path. The maildir root is INBOX. Subfolders follow either the Maildir++
// layout used by Dovecot/cPanel, where ".Work.Projects" is a single
// dot-prefixed directory encoding hierarchy with dots, or a plain nested
// (fs) layout. Local hierarchy uses "/", matching the IMAP LIST delimiter.
func maildirFolderName(root string, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." {
		return "INBOX"
	}
	var segments []string
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if trimmed := strings.TrimPrefix(part, "."); strings.HasPrefix(part, ".") && trimmed != "" {
			segments = append(segments, strings.Split(trimmed, ".")...)
		} else if part != "" {
			segments = append(segments, part)
		}
	}
	if len(segments) == 0 {
		return "INBOX"
	}
	if len(segments) > 1 && strings.EqualFold(segments[0], "INBOX") {
		segments = segments[1:]
	}
	segments[0] = canonicalFolderSegment(segments[0])
	return strings.Join(segments, "/")
}

// maildirFlags parses the ":2,<flags>" suffix of a maildir filename.
func maildirFlags(filename string) importFlags {
	idx := strings.LastIndex(filename, ":2,")
	if idx < 0 {
		return importFlags{}
	}
	var flags importFlags
	for _, r := range filename[idx+3:] {
		switch r {
		case 'S':
			flags.read = true
		case 'F':
			flags.flagged = true
		case 'T':
			flags.deleted = true
		}
	}
	return flags
}

func readZipFile(file *zip.File) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func messageObjectKey(t time.Time, messageID string) string {
	return fmt.Sprintf("messages/%04d/%02d/%02d/%s.eml", t.Year(), t.Month(), t.Day(), messageID)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
