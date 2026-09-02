package api

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/gofiber/fiber/v3"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/label"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessagelabel"
	"github.com/zephyraoss/haitatsu/internal/database/ent/predicate"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
)

type mailboxMessageUpdateRequest struct {
	Read     *bool    `json:"read"`
	Flagged  *bool    `json:"flagged"`
	Answered *bool    `json:"answered"`
	Draft    *bool    `json:"draft"`
	Keywords []string `json:"keywords"`
}

type moveMessageRequest struct {
	FolderID string `json:"folder_id"`
}

type messageLabelRequest struct {
	LabelID string `json:"label_id"`
}

type mailboxMessageResponse struct {
	MailboxMessage *ent.MailboxMessage `json:"mailbox_message"`
	Message        *ent.Message        `json:"message"`
	Labels         []*ent.Label        `json:"labels"`
}

const messageSearchVector = "to_tsvector('english', coalesce(subject, '') || ' ' || coalesce(from_addresses::text, '') || ' ' || coalesce(to_addresses::text, '') || ' ' || coalesce(cc_addresses::text, '') || ' ' || coalesce(text_body_extract, '') || ' ' || coalesce(html_body_extract, '') || ' ' || coalesce(attachments::text, ''))"

type messageSearch struct {
	Terms         []string
	From          string
	To            string
	CC            string
	Subject       string
	Label         string
	Folder        string
	HasAttachment bool
	Read          *bool
	Flagged       *bool
	Tag           string
	After         *time.Time
	Before        *time.Time
}

func (h *Handler) listMessages(c fiber.Ctx) error {
	limit := requestLimit(c)
	cur, hasCursor, ok := requestCursor(c)
	if !ok {
		return problem(c, fiber.StatusBadRequest, "invalid_cursor", "Cursor is invalid")
	}
	query := h.client.MailboxMessage.Query().
		Where(mailboxmessage.MailboxIDEQ(c.Params("mailbox_id")), mailboxmessage.DeletedAtIsNil()).
		Order(mailboxmessage.ByCreatedAt(entsql.OrderDesc()), mailboxmessage.ByID(entsql.OrderDesc())).
		Limit(limit + 1)
	if hasCursor {
		query.Where(cursorPredicate[predicate.MailboxMessage](mailboxmessage.FieldCreatedAt, mailboxmessage.FieldID, cur))
	}

	if folderID := c.Query("folder_id"); folderID != "" {
		query.Where(mailboxmessage.FolderIDEQ(folderID))
	}
	if read := c.Query("read"); read != "" {
		query.Where(mailboxmessage.ReadEQ(read == "true"))
	}
	if flagged := c.Query("flagged"); flagged != "" {
		query.Where(mailboxmessage.FlaggedEQ(flagged == "true"))
	}
	if labelID := c.Query("label_id"); labelID != "" {
		ids, err := h.mailboxMessageIDsForLabel(c, labelID)
		if err != nil {
			return problem(c, fiber.StatusInternalServerError, "message_label_filter_failed", "Failed to filter messages by label")
		}
		query.Where(mailboxmessage.IDIn(ids...))
	}
	if rawSearch := strings.TrimSpace(c.Query("q")); rawSearch != "" {
		search := parseMessageSearch(rawSearch)
		if err := h.applyMailboxSearch(c, query, search); err != nil {
			return problem(c, fiber.StatusBadRequest, "invalid_message_search", err.Error())
		}
		ids, err := h.searchMessageIDs(c, search)
		if err != nil {
			return problem(c, fiber.StatusInternalServerError, "message_search_failed", "Failed to search messages")
		}
		if ids != nil {
			query.Where(mailboxmessage.MessageIDIn(ids...))
		}
	}

	items, err := query.All(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "message_list_failed", "Failed to list messages")
	}
	page, next := nextCursor(items, limit, func(item *ent.MailboxMessage) (string, string) { return cursorTime(item.CreatedAt), item.ID })
	responses, err := h.mailboxMessageResponses(c, page)
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "message_load_failed", "Failed to load messages")
	}
	return list(c, responses, limit, next)
}

func (h *Handler) getMessage(c fiber.Ctx) error {
	item, err := h.client.MailboxMessage.Get(c.Context(), c.Params("id"))
	if err != nil {
		return entProblem(c, err, "message_not_found", "Message not found")
	}
	response, err := h.mailboxMessageResponse(c, item)
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "message_load_failed", "Failed to load message")
	}
	return data(c, response)
}

func (h *Handler) downloadRawMessage(c fiber.Ctx) error {
	msg, err := h.messageForMailboxMessage(c)
	if err != nil {
		return err
	}
	raw, err := h.store.GetMessage(c.Context(), msg.BlobKey)
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "raw_message_download_failed", "Failed to download raw message")
	}
	c.Set("Content-Type", "message/rfc822")
	c.Set("Content-Disposition", `attachment; filename="`+msg.ID+`.eml"`)
	return c.Send(raw)
}

func (h *Handler) downloadAttachment(c fiber.Ctx) error {
	partIndex, err := strconv.Atoi(c.Params("attachment_id"))
	if err != nil || partIndex <= 0 {
		return problem(c, fiber.StatusBadRequest, "invalid_attachment", "Attachment ID must be a positive part index")
	}
	msg, handlerErr := h.messageForMailboxMessage(c)
	if handlerErr != nil {
		return handlerErr
	}
	raw, err := h.store.GetMessage(c.Context(), msg.BlobKey)
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "attachment_download_failed", "Failed to download attachment")
	}
	attachment, err := mailparse.ExtractAttachment(raw, partIndex)
	if err != nil {
		return problem(c, fiber.StatusNotFound, "attachment_not_found", "Attachment not found")
	}
	if attachment.ContentType != "" {
		c.Set("Content-Type", attachment.ContentType)
	} else {
		c.Set("Content-Type", "application/octet-stream")
	}
	if attachment.Filename != "" {
		c.Set("Content-Disposition", `attachment; filename="`+safeFilename(attachment.Filename)+`"`)
	}
	return c.Send(attachment.Data)
}

func (h *Handler) updateMailboxMessage(c fiber.Ctx) error {
	var req mailboxMessageUpdateRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	item, err := h.client.MailboxMessage.Get(c.Context(), c.Params("id"))
	if err != nil {
		return entProblem(c, err, "message_not_found", "Message not found")
	}
	flags := mailstore.FlagsOf(item)
	if req.Read != nil {
		flags.Seen = *req.Read
	}
	if req.Flagged != nil {
		flags.Flagged = *req.Flagged
	}
	if req.Answered != nil {
		flags.Answered = *req.Answered
	}
	if req.Draft != nil {
		flags.Draft = *req.Draft
	}
	if req.Keywords != nil {
		flags.Keywords = req.Keywords
	}
	updated, err := h.mail.SetFlags(c.Context(), item, flags)
	if err != nil {
		return entProblem(c, err, "message_update_failed", "Failed to update message")
	}
	return data(c, updated)
}

func (h *Handler) messageForMailboxMessage(c fiber.Ctx) (*ent.Message, error) {
	item, err := h.client.MailboxMessage.Get(c.Context(), c.Params("id"))
	if err != nil {
		return nil, entProblem(c, err, "message_not_found", "Message not found")
	}
	msg, err := h.client.Message.Get(c.Context(), item.MessageID)
	if err != nil {
		return nil, entProblem(c, err, "message_not_found", "Message not found")
	}
	return msg, nil
}

func (h *Handler) moveMessage(c fiber.Ctx) error {
	var req moveMessageRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	if req.FolderID == "" {
		return problem(c, fiber.StatusBadRequest, "folder_id_required", "Folder ID is required")
	}
	item, err := h.client.MailboxMessage.Get(c.Context(), c.Params("id"))
	if err != nil {
		return entProblem(c, err, "message_not_found", "Message not found")
	}
	folder, err := h.client.Folder.Get(c.Context(), req.FolderID)
	if err != nil {
		return entProblem(c, err, "folder_not_found", "Folder not found")
	}
	if folder.MailboxID != item.MailboxID {
		return problem(c, fiber.StatusBadRequest, "folder_mailbox_mismatch", "Folder does not belong to message mailbox")
	}
	item, err = h.mail.Move(c.Context(), item, req.FolderID)
	if err != nil {
		return entProblem(c, err, "message_move_failed", "Failed to move message")
	}
	return data(c, item)
}

func (h *Handler) deleteMessage(c fiber.Ctx) error {
	if c.Query("force") == "true" {
		return h.permanentlyDeleteMessage(c)
	}
	item, err := h.moveToSystemFolder(c, "Trash")
	if err != nil {
		return err
	}
	return data(c, item)
}

func (h *Handler) restoreMessage(c fiber.Ctx) error {
	item, err := h.moveToSystemFolder(c, "INBOX")
	if err != nil {
		return err
	}
	return data(c, item)
}

func (h *Handler) permanentlyDeleteMessage(c fiber.Ctx) error {
	item, err := h.client.MailboxMessage.Get(c.Context(), c.Params("id"))
	if err != nil {
		return entProblem(c, err, "message_not_found", "Message not found")
	}
	if err := h.mail.SoftDelete(c.Context(), item); err != nil {
		return entProblem(c, err, "message_delete_failed", "Failed to permanently delete message")
	}
	item, err = h.client.MailboxMessage.Get(c.Context(), item.ID)
	if err != nil {
		return entProblem(c, err, "message_not_found", "Message not found")
	}
	return data(c, item)
}

func (h *Handler) addMessageLabel(c fiber.Ctx) error {
	var req messageLabelRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	if req.LabelID == "" {
		return problem(c, fiber.StatusBadRequest, "label_id_required", "Label ID is required")
	}
	item, err := h.client.MailboxMessage.Get(c.Context(), c.Params("id"))
	if err != nil {
		return entProblem(c, err, "message_not_found", "Message not found")
	}
	label, err := h.client.Label.Get(c.Context(), req.LabelID)
	if err != nil {
		return entProblem(c, err, "label_not_found", "Label not found")
	}
	if label.MailboxID != item.MailboxID {
		return problem(c, fiber.StatusBadRequest, "label_mailbox_mismatch", "Label does not belong to message mailbox")
	}
	link, err := h.mail.AddLabel(c.Context(), item, req.LabelID)
	if err != nil {
		return entProblem(c, err, "message_label_failed", "Failed to add message label")
	}
	return created(c, link)
}

func (h *Handler) removeMessageLabel(c fiber.Ctx) error {
	item, err := h.client.MailboxMessage.Get(c.Context(), c.Params("id"))
	if err != nil {
		return entProblem(c, err, "message_not_found", "Message not found")
	}
	if err := h.mail.RemoveLabel(c.Context(), item, c.Params("label_id")); err != nil {
		return problem(c, fiber.StatusInternalServerError, "message_label_remove_failed", "Failed to remove message label")
	}
	return empty(c)
}

func (h *Handler) moveToSystemFolder(c fiber.Ctx, name string) (*ent.MailboxMessage, error) {
	item, err := h.client.MailboxMessage.Get(c.Context(), c.Params("id"))
	if err != nil {
		return nil, entProblem(c, err, "message_not_found", "Message not found")
	}
	moved, err := h.mail.MoveToFolderName(c.Context(), item, name)
	if err != nil {
		return nil, entProblem(c, err, "folder_not_found", "Folder not found")
	}
	return moved, nil
}

func (h *Handler) mailboxMessageResponses(c fiber.Ctx, items []*ent.MailboxMessage) ([]mailboxMessageResponse, error) {
	responses := make([]mailboxMessageResponse, 0, len(items))
	for _, item := range items {
		response, err := h.mailboxMessageResponse(c, item)
		if err != nil {
			return nil, err
		}
		responses = append(responses, response)
	}
	return responses, nil
}

func (h *Handler) mailboxMessageResponse(c fiber.Ctx, item *ent.MailboxMessage) (mailboxMessageResponse, error) {
	msg, err := h.client.Message.Get(c.Context(), item.MessageID)
	if err != nil {
		return mailboxMessageResponse{}, err
	}
	labels, err := h.labelsForMailboxMessage(c, item.ID)
	if err != nil {
		return mailboxMessageResponse{}, err
	}
	return mailboxMessageResponse{MailboxMessage: item, Message: msg, Labels: labels}, nil
}

func (h *Handler) labelsForMailboxMessage(c fiber.Ctx, mailboxMessageID string) ([]*ent.Label, error) {
	links, err := h.client.MailboxMessageLabel.Query().Where(mailboxmessagelabel.MailboxMessageIDEQ(mailboxMessageID)).All(c.Context())
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(links))
	for _, link := range links {
		ids = append(ids, link.LabelID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return h.client.Label.Query().Where(label.IDIn(ids...)).All(c.Context())
}

func (h *Handler) mailboxMessageIDsForLabel(c fiber.Ctx, labelID string) ([]string, error) {
	links, err := h.client.MailboxMessageLabel.Query().Where(mailboxmessagelabel.LabelIDEQ(labelID)).All(c.Context())
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(links))
	for _, link := range links {
		ids = append(ids, link.MailboxMessageID)
	}
	return ids, nil
}

func parseMessageSearch(value string) messageSearch {
	var search messageSearch
	for _, token := range searchTokens(value) {
		key, raw, ok := strings.Cut(token, ":")
		if !ok {
			search.Terms = append(search.Terms, token)
			continue
		}
		raw = strings.Trim(raw, `"`)
		switch strings.ToLower(key) {
		case "from":
			search.From = raw
		case "to":
			search.To = raw
		case "cc":
			search.CC = raw
		case "subject":
			search.Subject = raw
		case "label":
			search.Label = raw
		case "folder":
			search.Folder = raw
		case "has":
			search.HasAttachment = raw == "attachment"
		case "is":
			if raw == "read" {
				search.Read = boolPtr(true)
			}
			if raw == "unread" {
				search.Read = boolPtr(false)
			}
			if raw == "flagged" {
				search.Flagged = boolPtr(true)
			}
		case "tag":
			search.Tag = raw
		case "after":
			search.After = parseSearchDate(raw)
		case "before":
			search.Before = parseSearchDate(raw)
		default:
			search.Terms = append(search.Terms, token)
		}
	}
	return search
}

func (h *Handler) applyMailboxSearch(c fiber.Ctx, query *ent.MailboxMessageQuery, search messageSearch) error {
	if search.Read != nil {
		query.Where(mailboxmessage.ReadEQ(*search.Read))
	}
	if search.Flagged != nil {
		query.Where(mailboxmessage.FlaggedEQ(*search.Flagged))
	}
	if search.Tag != "" {
		query.Where(mailboxmessage.PlusTagEQ(search.Tag))
	}
	if search.Folder != "" {
		folder, err := h.client.Folder.Query().Where(folder.MailboxIDEQ(c.Params("mailbox_id")), folder.NameEQ(search.Folder)).Only(c.Context())
		if err != nil {
			return fmt.Errorf("folder not found")
		}
		query.Where(mailboxmessage.FolderIDEQ(folder.ID))
	}
	if search.Label != "" {
		label, err := h.client.Label.Query().Where(label.MailboxIDEQ(c.Params("mailbox_id")), label.NameEQ(search.Label)).Only(c.Context())
		if err != nil {
			return fmt.Errorf("label not found")
		}
		ids, err := h.mailboxMessageIDsForLabel(c, label.ID)
		if err != nil {
			return err
		}
		query.Where(mailboxmessage.IDIn(ids...))
	}
	return nil
}

func (h *Handler) searchMessageIDs(c fiber.Ctx, search messageSearch) ([]string, error) {
	where, args := messageSearchWhere(search)
	if len(where) == 0 {
		return nil, nil
	}
	query := "SELECT id FROM messages WHERE " + strings.Join(where, " AND ") + " ORDER BY created_at DESC"
	if len(search.Terms) > 0 {
		query = "SELECT id FROM messages, websearch_to_tsquery('english', $1) query WHERE " + strings.Join(where, " AND ") + " ORDER BY ts_rank(" + messageSearchVector + ", query) DESC, created_at DESC"
	}
	rows, err := h.db.QueryContext(c.Context(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func messageSearchWhere(search messageSearch) ([]string, []any) {
	var where []string
	var args []any
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if len(search.Terms) > 0 {
		args = append(args, strings.Join(search.Terms, " "))
		where = append(where, messageSearchVector+" @@ query")
	}
	if search.From != "" {
		add("from_addresses::text ILIKE '%%' || $%d || '%%'", search.From)
	}
	if search.To != "" {
		add("to_addresses::text ILIKE '%%' || $%d || '%%'", search.To)
	}
	if search.CC != "" {
		add("cc_addresses::text ILIKE '%%' || $%d || '%%'", search.CC)
	}
	if search.Subject != "" {
		add("subject ILIKE '%%' || $%d || '%%'", search.Subject)
	}
	if search.HasAttachment {
		where = append(where, "jsonb_array_length(attachments) > 0")
	}
	if search.After != nil {
		add("coalesce(date, created_at) >= $%d", *search.After)
	}
	if search.Before != nil {
		add("coalesce(date, created_at) <= $%d", *search.Before)
	}
	return where, args
}

func searchTokens(value string) []string {
	var tokens []string
	var current strings.Builder
	inQuote := false
	for _, char := range value {
		switch {
		case char == '"':
			inQuote = !inQuote
			current.WriteRune(char)
		case char == ' ' && !inQuote:
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(char)
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

func parseSearchDate(value string) *time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return &parsed
		}
	}
	return nil
}

func boolPtr(value bool) *bool {
	return &value
}

func safeFilename(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, `"`, "'")
	return value
}
