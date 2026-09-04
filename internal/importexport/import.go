package importexport

import (
	"archive/zip"
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

	"github.com/zephyraoss/haitatsu/internal/database"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/importjob"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailbox"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	messagepred "github.com/zephyraoss/haitatsu/internal/database/ent/message"
	"github.com/zephyraoss/haitatsu/internal/events"
	"github.com/zephyraoss/haitatsu/internal/ids"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
)

const (
	jobLeaseDuration      = 10 * time.Minute
	jobLeaseRenewInterval = 2 * time.Minute
)

type ImportWorker struct {
	db       *sql.DB
	client   *ent.Client
	store    Store
	mail     *mailstore.Store
	events   *events.Service
	workerID string
	backend  database.Backend
}

type importJob struct {
	ID         string
	MailboxID  string
	SourceType string
	Source     map[string]any
}

func NewImportWorker(db *sql.DB, client *ent.Client, store Store, mail *mailstore.Store, events *events.Service, workerID string, backends ...database.Backend) *ImportWorker {
	backend := database.BackendPostgres
	if len(backends) > 0 {
		backend = backends[0]
	}
	return &ImportWorker{db: db, client: client, store: store, mail: mail, events: events, workerID: workerID, backend: backend}
}

func (w *ImportWorker) mailStore() *mailstore.Store {
	if w.mail == nil {
		w.mail = mailstore.New(w.client, nil)
	}
	return w.mail
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
	job, owner, ok, err := w.claim(ctx)
	if err != nil || !ok {
		return err
	}
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.renewLease(jobCtx, cancel, job.ID, owner)
	count, err := w.importJob(jobCtx, job)
	if err != nil {
		if jobCtx.Err() != nil {
			w.release(job.ID, owner)
			return err
		}
		return w.fail(ctx, job, owner, count, err)
	}
	n, err := w.client.ImportJob.Update().
		Where(importjob.IDEQ(job.ID), importjob.LockedByEQ(owner)).
		SetStatus("completed").SetImportedCount(count).SetSource(ScrubSource(job.Source)).ClearLockedBy().ClearLockedUntil().
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	return w.events.Emit(ctx, events.MailboxImportCompleted, job.MailboxID, map[string]any{"event": events.MailboxImportCompleted, "import_id": job.ID, "mailbox_id": job.MailboxID, "imported_count": count})
}

func (w *ImportWorker) claim(ctx context.Context) (importJob, string, bool, error) {
	owner := w.workerID + "/" + ids.New().String()
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return importJob{}, "", false, err
	}
	defer tx.Rollback()

	query := `
UPDATE import_jobs SET locked_by = $1, locked_until = $2, status = 'processing', updated_at = NOW()
WHERE id = (
  SELECT id FROM import_jobs
  WHERE status = 'queued' AND (locked_until IS NULL OR locked_until <= NOW())
  ORDER BY created_at
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING id, mailbox_id, source_type, source
`
	now := time.Now().UTC()
	args := []any{owner, now.Add(jobLeaseDuration)}
	if w.backend.SQLiteFamily() {
		query = `
UPDATE import_jobs SET locked_by = ?, locked_until = ?, status = 'processing', updated_at = ?
WHERE id = (
  SELECT id FROM import_jobs
  WHERE status = 'queued' AND (locked_until IS NULL OR datetime(locked_until) <= datetime(?))
  ORDER BY created_at
  LIMIT 1
)
AND status = 'queued' AND (locked_until IS NULL OR datetime(locked_until) <= datetime(?))
RETURNING id, mailbox_id, source_type, source
`
		args = []any{owner, now.Add(jobLeaseDuration), now, now, now}
	}
	row := tx.QueryRowContext(ctx, query, args...)

	var job importJob
	var source []byte
	if err := row.Scan(&job.ID, &job.MailboxID, &job.SourceType, &source); err != nil {
		if err == sql.ErrNoRows {
			return importJob{}, "", false, nil
		}
		return importJob{}, "", false, err
	}
	if err := json.Unmarshal(source, &job.Source); err != nil {
		return importJob{}, "", false, err
	}
	if err := tx.Commit(); err != nil {
		return importJob{}, "", false, err
	}
	return job, owner, true, nil
}

func (w *ImportWorker) renewLease(ctx context.Context, cancel context.CancelFunc, jobID string, owner string) {
	ticker := time.NewTicker(jobLeaseRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := w.client.ImportJob.Update().
				Where(importjob.IDEQ(jobID), importjob.StatusEQ("processing"), importjob.LockedByEQ(owner)).
				SetLockedUntil(time.Now().Add(jobLeaseDuration)).
				Save(ctx)
			if err != nil {
				continue
			}
			if n == 0 {
				cancel()
				return
			}
		}
	}
}

func (w *ImportWorker) release(jobID string, owner string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = w.client.ImportJob.Update().
		Where(importjob.IDEQ(jobID), importjob.LockedByEQ(owner)).
		SetStatus("queued").ClearLockedBy().ClearLockedUntil().
		Save(ctx)
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
	progress := &importProgress{client: w.client, jobID: job.ID}
	switch job.SourceType {
	case "zip":
		err = w.importZip(ctx, job, mbox, inbox.ID, progress)
	case "maildir":
		err = w.importMaildir(ctx, job, mbox, folders, progress)
	case "imap":
		err = w.importIMAP(ctx, job, mbox, folders, progress)
	default:
		err = fmt.Errorf("unsupported import source type %q", job.SourceType)
	}
	progress.flush(ctx)
	return progress.imported, err
}

const progressFlushEvery = 25

type importProgress struct {
	client   *ent.Client
	jobID    string
	imported int
}

func (p *importProgress) recordCreated(ctx context.Context) {
	p.imported++
	if p.imported%progressFlushEvery == 0 {
		p.flush(ctx)
	}
}

func (p *importProgress) flush(ctx context.Context) {
	if p.jobID == "" || p.imported == 0 {
		return
	}
	_, _ = p.client.ImportJob.UpdateOneID(p.jobID).SetImportedCount(p.imported).Save(ctx)
}

type importFlags struct {
	read     bool
	flagged  bool
	deleted  bool
	answered bool
	draft    bool
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
	created, err := w.mailStore().CreateFolder(ctx, mailboxID, name, false)
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

func (w *ImportWorker) importZip(ctx context.Context, job importJob, mbox *ent.Mailbox, inboxID string, progress *importProgress) error {
	key := sourceKey(job.Source)
	if key == "" {
		return fmt.Errorf("zip import requires source.object_key")
	}
	reader, err := w.store.GetObjectReader(ctx, key)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "haitatsu-import-*.zip")
	if err != nil {
		reader.Close()
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	size, err := io.Copy(tmp, reader)
	reader.Close()
	if err != nil {
		return err
	}
	archive, err := zip.NewReader(tmp, size)
	if err != nil {
		return err
	}

	for _, file := range archive.File {
		if file.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(file.Name), ".eml") {
			continue
		}
		raw, err := readZipFile(file)
		if err != nil {
			return err
		}
		created, err := w.addToMailbox(ctx, job.MailboxID, inboxID, mbox.PrimaryAddress, raw, importFlags{})
		if err != nil {
			return err
		}
		if created {
			progress.recordCreated(ctx)
		}
	}
	return nil
}

func (w *ImportWorker) importMaildir(ctx context.Context, job importJob, mbox *ent.Mailbox, folders *folderCache, progress *importProgress) error {
	root := strings.TrimSpace(sourceString(job.Source, "path"))
	if root == "" {
		return fmt.Errorf("maildir import requires source.path")
	}
	entries, err := maildirEntries(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(entry.path)
		if err != nil {
			return err
		}
		folderID, err := w.ensureFolder(ctx, folders, job.MailboxID, entry.folder)
		if err != nil {
			return err
		}
		created, err := w.addToMailbox(ctx, job.MailboxID, folderID, mbox.PrimaryAddress, raw, entry.flags)
		if err != nil {
			return err
		}
		if created {
			progress.recordCreated(ctx)
		}
	}
	return nil
}

func (w *ImportWorker) importIMAP(ctx context.Context, job importJob, mbox *ent.Mailbox, folders *folderCache, progress *importProgress) error {
	client, err := dialIMAP(job.Source)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.Login(sourceString(job.Source, "username"), sourceString(job.Source, "password")).Wait(); err != nil {
		return err
	}
	mailboxes := sourceStringSlice(job.Source, "mailboxes")
	if len(mailboxes) == 0 {
		if mailboxName := sourceString(job.Source, "mailbox"); mailboxName != "" {
			mailboxes = []string{mailboxName}
		}
	}
	delim := remoteDelimiter(client)
	if len(mailboxes) == 0 {
		mailboxes, delim, err = listRemoteMailboxes(client)
		if err != nil {
			return err
		}
	}
	for _, mailboxName := range mailboxes {
		folderID, err := w.ensureFolder(ctx, folders, job.MailboxID, destinationFolderName(mailboxName, delim))
		if err != nil {
			return err
		}
		if err := w.importIMAPMailbox(ctx, client, mailboxName, job.MailboxID, folderID, mbox.PrimaryAddress, progress); err != nil {
			return err
		}
	}
	return nil
}

func remoteDelimiter(client *imapclient.Client) string {
	items, err := client.List("", "", nil).Collect()
	if err != nil || len(items) == 0 || items[0].Delim == 0 {
		return "/"
	}
	return string(items[0].Delim)
}

func listRemoteMailboxes(client *imapclient.Client) ([]string, string, error) {
	items, err := client.List("", "*", nil).Collect()
	if err != nil {
		return nil, "", err
	}
	delim := "/"
	names := make([]string, 0, len(items))
	for _, item := range items {
		if item.Delim != 0 {
			delim = string(item.Delim)
		}
		if hasMailboxAttr(item.Attrs, imap.MailboxAttrNoSelect) {
			continue
		}
		names = append(names, item.Mailbox)
	}
	if len(names) == 0 {
		names = []string{"INBOX"}
	}
	return names, delim, nil
}

func hasMailboxAttr(attrs []imap.MailboxAttr, expected imap.MailboxAttr) bool {
	for _, attr := range attrs {
		if attr == expected {
			return true
		}
	}
	return false
}

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

const imapFetchBatchSize = 200

func (w *ImportWorker) importIMAPMailbox(ctx context.Context, client *imapclient.Client, mailboxName string, mailboxID string, folderID string, primaryAddress string, progress *importProgress) error {
	selected, err := client.Select(mailboxName, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return err
	}
	for start := uint32(1); start <= selected.NumMessages; start += imapFetchBatchSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+imapFetchBatchSize-1, selected.NumMessages)
		if err := w.fetchIMAPBatch(ctx, client, start, end, mailboxID, folderID, primaryAddress, progress); err != nil {
			return err
		}
	}
	return nil
}

func (w *ImportWorker) fetchIMAPBatch(ctx context.Context, client *imapclient.Client, start uint32, end uint32, mailboxID string, folderID string, primaryAddress string, progress *importProgress) error {
	bodySection := &imap.FetchItemBodySection{}
	var seqSet imap.SeqSet
	seqSet.AddRange(start, end)
	fetch := client.Fetch(seqSet, &imap.FetchOptions{Flags: true, BodySection: []*imap.FetchItemBodySection{bodySection}})
	closeWithError := func(err error) error {
		_ = fetch.Close()
		return err
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
		created, err := w.addToMailbox(ctx, mailboxID, folderID, primaryAddress, raw, flags)
		if err != nil {
			return closeWithError(err)
		}
		if created {
			progress.recordCreated(ctx)
		}
	}
	return fetch.Close()
}

func (w *ImportWorker) addToMailbox(ctx context.Context, mailboxID string, folderID string, primaryAddress string, raw []byte, flags importFlags) (bool, error) {
	sha := sha256Hex(raw)
	attached, err := w.alreadyAttached(ctx, mailboxID, sha)
	if err != nil {
		return false, err
	}
	if attached {
		return false, nil
	}
	msg, err := w.messageForRaw(ctx, sha, raw)
	if err != nil {
		return false, err
	}
	return w.attachMessage(ctx, mailboxID, folderID, primaryAddress, msg.ID, int64(len(raw)), flags)
}

func (w *ImportWorker) alreadyAttached(ctx context.Context, mailboxID string, sha string) (bool, error) {
	messageIDs, err := w.client.Message.Query().Where(messagepred.Sha256EQ(sha)).IDs(ctx)
	if err != nil {
		return false, err
	}
	if len(messageIDs) == 0 {
		return false, nil
	}
	return w.client.MailboxMessage.Query().
		Where(mailboxmessage.MailboxIDEQ(mailboxID), mailboxmessage.MessageIDIn(messageIDs...)).
		Exist(ctx)
}

func (w *ImportWorker) attachMessage(ctx context.Context, mailboxID string, folderID string, primaryAddress string, messageID string, size int64, flags importFlags) (bool, error) {
	_, err := w.mailStore().Attach(ctx, mailstore.Attach{
		MailboxID:    mailboxID,
		MessageID:    messageID,
		FolderID:     folderID,
		SizeBytes:    size,
		OriginalRcpt: primaryAddress,
		BaseRcpt:     primaryAddress,
		Flags:        mailstore.Flags{Seen: flags.read, Flagged: flags.flagged, Deleted: flags.deleted, Answered: flags.answered, Draft: flags.draft},
	})
	if ent.IsConstraintError(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (w *ImportWorker) messageForRaw(ctx context.Context, sha string, raw []byte) (*ent.Message, error) {
	existing, err := w.client.Message.Query().Where(messagepred.Sha256EQ(sha)).Order(ent.Asc(messagepred.FieldID)).First(ctx)
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

func (w *ImportWorker) fail(ctx context.Context, job importJob, owner string, count int, cause error) error {
	n, err := w.client.ImportJob.Update().
		Where(importjob.IDEQ(job.ID), importjob.LockedByEQ(owner)).
		SetStatus("failed").SetImportedCount(count).SetLastError(map[string]any{"error": cause.Error()}).SetSource(ScrubSource(job.Source)).ClearLockedBy().ClearLockedUntil().
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
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
				case imap.FlagAnswered:
					flags.answered = true
				case imap.FlagDraft:
					flags.draft = true
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
		case 'R':
			flags.answered = true
		case 'D':
			flags.draft = true
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
