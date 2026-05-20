package api

import (
	"bytes"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/zephyraoss/haitatsu/internal/outbound"
)

type outboundMessageRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	CC      []string `json:"cc"`
	BCC     []string `json:"bcc"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
	HTML    string   `json:"html"`
	Raw     string   `json:"raw"`
}

func (h *Handler) createOutboundMessage(c fiber.Ctx) error {
	var req outboundMessageRequest
	if err := c.Bind().JSON(&req); err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	raw, err := req.messageBytes()
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "invalid_outbound_message", err.Error())
	}
	msg, err := h.outbound.Submit(c.Context(), c.Params("mailbox_id"), req.From, raw)
	if err != nil {
		return outboundProblem(c, err)
	}
	return created(c, msg)
}

func (r outboundMessageRequest) messageBytes() ([]byte, error) {
	if r.Raw != "" {
		if r.From == "" {
			return nil, errors.New("from is required")
		}
		return []byte(r.Raw), nil
	}
	if r.From == "" || len(r.To) == 0 {
		return nil, errors.New("from and to are required")
	}
	if _, err := mail.ParseAddress(r.From); err != nil {
		return nil, errors.New("from must be a valid email address")
	}
	if err := validateAddresses(r.To); err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	if err := validateAddresses(r.CC); err != nil {
		return nil, fmt.Errorf("cc: %w", err)
	}
	if err := validateAddresses(r.BCC); err != nil {
		return nil, fmt.Errorf("bcc: %w", err)
	}
	return buildMessage(r), nil
}

func buildMessage(req outboundMessageRequest) []byte {
	var message bytes.Buffer
	writeHeader(&message, "From", req.From)
	writeHeader(&message, "To", strings.Join(req.To, ", "))
	if len(req.CC) > 0 {
		writeHeader(&message, "Cc", strings.Join(req.CC, ", "))
	}
	writeHeader(&message, "Subject", req.Subject)
	if req.HTML != "" {
		boundary := "haitatsu-alt"
		writeHeader(&message, "MIME-Version", "1.0")
		writeHeader(&message, "Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
		message.WriteString("\r\n")
		writePart(&message, boundary, "text/plain; charset=utf-8", req.Text)
		writePart(&message, boundary, "text/html; charset=utf-8", req.HTML)
		message.WriteString("--" + boundary + "--\r\n")
		return message.Bytes()
	}
	writeHeader(&message, "Content-Type", "text/plain; charset=utf-8")
	message.WriteString("\r\n")
	message.WriteString(req.Text)
	return message.Bytes()
}

func writePart(message *bytes.Buffer, boundary string, contentType string, body string) {
	message.WriteString("--" + boundary + "\r\n")
	writeHeader(message, "Content-Type", contentType)
	message.WriteString("\r\n")
	message.WriteString(body)
	message.WriteString("\r\n")
}

func writeHeader(message *bytes.Buffer, key string, value string) {
	message.WriteString(key + ": " + strings.ReplaceAll(value, "\r\n", " ") + "\r\n")
}

func validateAddresses(addresses []string) error {
	for _, address := range addresses {
		if _, err := mail.ParseAddress(address); err != nil {
			return fmt.Errorf("invalid address %q", address)
		}
	}
	return nil
}

func outboundProblem(c fiber.Ctx, err error) error {
	if errors.Is(err, outbound.ErrSenderNotAllowed) {
		return problem(c, fiber.StatusForbidden, "sender_not_allowed", "Sender is not allowed for mailbox")
	}
	if errors.Is(err, outbound.ErrDKIMKeyNotFound) {
		return problem(c, fiber.StatusConflict, "dkim_key_not_found", "DKIM key is not configured for sender domain")
	}
	return problem(c, fiber.StatusInternalServerError, "outbound_failed", "Failed to create outbound message")
}
