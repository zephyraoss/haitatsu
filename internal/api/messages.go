package api

import (
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/gofiber/fiber/v3"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/folder"
	"github.com/zephyraoss/haitatsu/internal/database/ent/label"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessagelabel"
)

type mailboxMessageUpdateRequest struct {
	Read    *bool `json:"read"`
	Flagged *bool `json:"flagged"`
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

func (h *Handler) listMessages(c fiber.Ctx) error {
	limit := requestLimit(c)
	query := h.client.MailboxMessage.Query().
		Where(mailboxmessage.MailboxIDEQ(c.Params("mailbox_id")), mailboxmessage.DeletedAtIsNil()).
		Order(mailboxmessage.ByCreatedAt(entsql.OrderDesc())).
		Limit(limit)

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
	if search := strings.TrimSpace(c.Query("q")); search != "" {
		ids, err := h.searchMessageIDs(c, search)
		if err != nil {
			return problem(c, fiber.StatusInternalServerError, "message_search_failed", "Failed to search messages")
		}
		query.Where(mailboxmessage.MessageIDIn(ids...))
	}

	items, err := query.All(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "message_list_failed", "Failed to list messages")
	}
	responses, err := h.mailboxMessageResponses(c, items)
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "message_load_failed", "Failed to load messages")
	}
	return list(c, responses, limit, "")
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
	item, err := h.client.MailboxMessage.Get(c.Context(), c.Params("id"))
	if err != nil {
		return entProblem(c, err, "message_not_found", "Message not found")
	}
	msg, err := h.client.Message.Get(c.Context(), item.MessageID)
	if err != nil {
		return entProblem(c, err, "message_not_found", "Message not found")
	}
	raw, err := h.store.GetMessage(c.Context(), msg.BlobKey)
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "raw_message_download_failed", "Failed to download raw message")
	}
	c.Set("Content-Type", "message/rfc822")
	c.Set("Content-Disposition", `attachment; filename="`+msg.ID+`.eml"`)
	return c.Send(raw)
}

func (h *Handler) updateMailboxMessage(c fiber.Ctx) error {
	var req mailboxMessageUpdateRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	update := h.client.MailboxMessage.UpdateOneID(c.Params("id"))
	if req.Read != nil {
		update.SetRead(*req.Read)
	}
	if req.Flagged != nil {
		update.SetFlagged(*req.Flagged)
	}
	item, err := update.Save(c.Context())
	if err != nil {
		return entProblem(c, err, "message_update_failed", "Failed to update message")
	}
	return data(c, item)
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
	item, err = h.client.MailboxMessage.UpdateOneID(item.ID).SetFolderID(req.FolderID).ClearDeletedAt().Save(c.Context())
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
	item, err := h.client.MailboxMessage.UpdateOneID(c.Params("id")).SetDeletedAt(time.Now()).Save(c.Context())
	if err != nil {
		return entProblem(c, err, "message_delete_failed", "Failed to permanently delete message")
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
	link, err := h.client.MailboxMessageLabel.Create().SetMailboxMessageID(item.ID).SetLabelID(req.LabelID).Save(c.Context())
	if err != nil {
		return entProblem(c, err, "message_label_failed", "Failed to add message label")
	}
	return created(c, link)
}

func (h *Handler) removeMessageLabel(c fiber.Ctx) error {
	_, err := h.client.MailboxMessageLabel.Delete().Where(
		mailboxmessagelabel.MailboxMessageIDEQ(c.Params("id")),
		mailboxmessagelabel.LabelIDEQ(c.Params("label_id")),
	).Exec(c.Context())
	if err != nil {
		return problem(c, fiber.StatusInternalServerError, "message_label_remove_failed", "Failed to remove message label")
	}
	return empty(c)
}

func (h *Handler) moveToSystemFolder(c fiber.Ctx, name string) (*ent.MailboxMessage, error) {
	item, err := h.client.MailboxMessage.Get(c.Context(), c.Params("id"))
	if err != nil {
		return nil, entProblem(c, err, "message_not_found", "Message not found")
	}
	folder, err := h.client.Folder.Query().Where(folder.MailboxIDEQ(item.MailboxID), folder.NameEQ(name)).Only(c.Context())
	if err != nil {
		return nil, entProblem(c, err, "folder_not_found", "Folder not found")
	}
	return h.client.MailboxMessage.UpdateOneID(item.ID).SetFolderID(folder.ID).ClearDeletedAt().Save(c.Context())
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

func (h *Handler) searchMessageIDs(c fiber.Ctx, search string) ([]string, error) {
	rows, err := h.db.QueryContext(c.Context(), `
SELECT id
FROM messages, websearch_to_tsquery('english', $1) query
WHERE to_tsvector('english', concat_ws(' ', subject, text_body_extract, html_body_extract, attachments::text)) @@ query
ORDER BY ts_rank(to_tsvector('english', concat_ws(' ', subject, text_body_extract, html_body_extract, attachments::text)), query) DESC, created_at DESC
`, search)
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
