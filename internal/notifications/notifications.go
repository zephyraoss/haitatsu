package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
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
	cfg      config.NotificationConfig
	from     string
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

func New(client *ent.Client, messages *messages.Service, cfg config.NotificationConfig, publicHostname string) *Service {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	from := strings.TrimSpace(cfg.FromAddress)
	if from == "" {
		from = "postmaster@" + publicHostname
	}
	return &Service{client: client, messages: messages, cfg: cfg, from: from, domain: publicHostname, http: &http.Client{Timeout: timeout}}
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
	raw := buildMessage(s.from, mbox.PrimaryAddress, s.domain, rendered)
	_, err = s.messages.Deliver(ctx, raw, []routing.Result{{OriginalRecipient: mbox.PrimaryAddress, BaseRecipient: mbox.PrimaryAddress, Mailboxes: []*ent.Mailbox{mbox}}}, spam.Assessment{})
	return err
}

func (s *Service) render(ctx context.Context, typ string, mailboxID string, messageID string, data map[string]any) renderedMessage {
	if s.cfg.RenderURL == "" {
		return fallback(typ, data)
	}
	body, err := json.Marshal(renderRequest{Type: typ, MailboxID: mailboxID, MessageID: messageID, Data: data})
	if err != nil {
		return fallback(typ, data)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.RenderURL, bytes.NewReader(body))
	if err != nil {
		return fallback(typ, data)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return fallback(typ, data)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fallback(typ, data)
	}
	var rendered renderedMessage
	if err := json.NewDecoder(resp.Body).Decode(&rendered); err != nil {
		return fallback(typ, data)
	}
	if rendered.Subject == "" || rendered.Text == "" {
		return fallback(typ, data)
	}
	return rendered
}

func fallback(typ string, data map[string]any) renderedMessage {
	subject := "Message delivery failed"
	text := "A message could not be delivered."
	if response, _ := data["response"].(string); response != "" {
		text = fmt.Sprintf("A message could not be delivered.\n\n%s", response)
	}
	return renderedMessage{Subject: subject, Text: text, HTML: "<p>" + html.EscapeString(text) + "</p>"}
}

func buildMessage(from string, to string, domain string, rendered renderedMessage) []byte {
	var buf bytes.Buffer
	buf.WriteString("From: " + formatAddress("Haitatsu", from) + "\r\n")
	buf.WriteString("To: " + formatAddress("", to) + "\r\n")
	buf.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", rendered.Subject) + "\r\n")
	buf.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	buf.WriteString("Message-ID: <" + ids.New().String() + "@" + domain + ">\r\n")
	buf.WriteString("MIME-Version: 1.0\r\n")
	if rendered.HTML != "" {
		boundary := "haitatsu-notification"
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
