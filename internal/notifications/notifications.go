package notifications

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"io"
	"mime"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/ids"
	"github.com/zephyraoss/haitatsu/internal/messages"
	"github.com/zephyraoss/haitatsu/internal/routing"
	"github.com/zephyraoss/haitatsu/internal/spam"
)

type Service struct {
	client   *ent.Client
	messages *messages.Service
	cfg      func() config.NotificationConfig
	domain   string
	http     *http.Client
}

type renderRequest struct {
	Type      string         `json:"type"`
	MailboxID string         `json:"mailbox_id"`
	MessageID string         `json:"message_id"`
	Data      map[string]any `json:"data"`
}

type renderedMessage struct {
	Subject string `json:"subject"`
	Text    string `json:"text"`
	HTML    string `json:"html"`
}

func New(client *ent.Client, messages *messages.Service, cfg func() config.NotificationConfig, publicHostname string) *Service {
	return &Service{client: client, messages: messages, cfg: cfg, domain: publicHostname, http: &http.Client{}}
}

func (s *Service) from() string {
	from := strings.TrimSpace(s.cfg().FromAddress)
	if from == "" {
		return "postmaster@" + s.domain
	}
	return from
}

func (s *Service) maxBytes() int64 {
	if value := s.cfg().MaxResponseBytes; value > 0 {
		return value
	}
	return 64 * 1024
}

func (s *Service) timeout() time.Duration {
	if value := s.cfg().TimeoutSeconds; value > 0 {
		return time.Duration(value) * time.Second
	}
	return 10 * time.Second
}

func (s *Service) OutboundFailure(ctx context.Context, mailboxID string, messageID string, data map[string]any) error {
	return s.deliver(ctx, "permanent_bounce", mailboxID, messageID, data)
}

func (s *Service) deliver(ctx context.Context, typ string, mailboxID string, messageID string, data map[string]any) error {
	mbox, err := s.client.Mailbox.Get(ctx, mailboxID)
	if err != nil {
		return err
	}
	rendered := s.render(ctx, typ, mailboxID, messageID, data)
	raw := buildMessage(s.from(), mbox.PrimaryAddress, s.domain, rendered)
	_, err = s.messages.Deliver(ctx, raw, []routing.Result{{OriginalRecipient: mbox.PrimaryAddress, BaseRecipient: mbox.PrimaryAddress, Mailboxes: []*ent.Mailbox{mbox}}}, spam.Assessment{})
	return err
}

func (s *Service) render(ctx context.Context, typ string, mailboxID string, messageID string, data map[string]any) renderedMessage {
	cfg := s.cfg()
	if cfg.RenderURL == "" {
		return fallback(typ, data)
	}
	body, err := json.Marshal(renderRequest{Type: typ, MailboxID: mailboxID, MessageID: messageID, Data: data})
	if err != nil {
		return fallback(typ, data)
	}
	reqCtx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.RenderURL, bytes.NewReader(body))
	if err != nil {
		return fallback(typ, data)
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.RenderSecret != "" {
		timestamp := time.Now().UTC().Format(time.RFC3339)
		req.Header.Set("X-Haitatsu-Notification", typ)
		req.Header.Set("X-Haitatsu-Timestamp", timestamp)
		req.Header.Set("X-Haitatsu-Signature", signature(cfg.RenderSecret, timestamp, body))
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fallback(typ, data)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fallback(typ, data)
	}
	var rendered renderedMessage
	maxBytes := s.maxBytes()
	response, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil || int64(len(response)) > maxBytes {
		return fallback(typ, data)
	}
	if err := json.Unmarshal(response, &rendered); err != nil {
		return fallback(typ, data)
	}
	if rendered.Subject == "" || rendered.Text == "" {
		return fallback(typ, data)
	}
	return rendered
}

func fallback(typ string, data map[string]any) renderedMessage {
	subject := fallbackSubject(typ)
	text := fallbackText(typ, data)
	return renderedMessage{Subject: subject, Text: text, HTML: htmlParagraphs(text)}
}

func fallbackSubject(typ string) string {
	switch typ {
	case "permanent_bounce":
		return "Message delivery failed"
	default:
		return "Mailbox notification"
	}
}

func fallbackText(typ string, data map[string]any) string {
	switch typ {
	case "permanent_bounce":
		text := "A message could not be delivered."
		if classification, _ := data["classification"].(string); classification != "" {
			text += "\n\nStatus: " + classification
		}
		if response, _ := data["response"].(string); response != "" {
			text += "\n\n" + response
		}
		return text
	default:
		return "Haitatsu generated a mailbox notification."
	}
}

func htmlParagraphs(text string) string {
	parts := strings.Split(text, "\n\n")
	var buf strings.Builder
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		buf.WriteString("<p>")
		buf.WriteString(html.EscapeString(part))
		buf.WriteString("</p>")
	}
	return buf.String()
}

func signature(secret string, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func buildMessage(from string, to string, domain string, rendered renderedMessage) []byte {
	boundary := "haitatsu-notification-" + ids.New().String()
	var buf bytes.Buffer
	buf.WriteString("From: " + formatAddress("Haitatsu", from) + "\r\n")
	buf.WriteString("To: " + formatAddress("", to) + "\r\n")
	buf.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", rendered.Subject) + "\r\n")
	buf.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	buf.WriteString("Message-ID: <" + ids.New().String() + "@" + domain + ">\r\n")
	buf.WriteString("MIME-Version: 1.0\r\n")
	if rendered.HTML != "" {
		buf.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")
		buf.WriteString("--" + boundary + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + rendered.Text + "\r\n")
		buf.WriteString("--" + boundary + "\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" + rendered.HTML + "\r\n")
		buf.WriteString("--" + boundary + "--\r\n")
		return buf.Bytes()
	}
	buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	buf.WriteString(rendered.Text)
	return buf.Bytes()
}

func formatAddress(defaultName string, address string) string {
	parsed, err := mail.ParseAddress(address)
	if err == nil {
		if parsed.Name == "" {
			parsed.Name = defaultName
		}
		return parsed.String()
	}
	return (&mail.Address{Name: defaultName, Address: address}).String()
}
