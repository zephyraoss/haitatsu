package inbound

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/zephyraoss/haitatsu/internal/bounce"
	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/messages"
	"github.com/zephyraoss/haitatsu/internal/routing"
)

type Server struct {
	server *smtp.Server
}

func New(cfg config.SMTPConfig, domain string, tlsConfig *tls.Config, resolver *routing.Resolver, messages *messages.Service, bounces *bounce.Handler) *Server {
	server := smtp.NewServer(&backend{resolver: resolver, messages: messages, bounces: bounces})
	server.Addr = cfg.InboundAddr
	server.Domain = domain
	server.TLSConfig = tlsConfig
	server.ReadTimeout = 10 * time.Second
	server.WriteTimeout = 10 * time.Second
	server.MaxMessageBytes = cfg.MaxMessageSizeBytes
	server.MaxRecipients = cfg.MaxInboundRecipients
	server.AllowInsecureAuth = false
	return &Server{server: server}
}

func (s *Server) Listen() error {
	err := s.server.ListenAndServe()
	if errors.Is(err, smtp.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

type backend struct {
	resolver *routing.Resolver
	messages *messages.Service
	bounces  *bounce.Handler
}

func (b *backend) NewSession(*smtp.Conn) (smtp.Session, error) {
	return &session{resolver: b.resolver, messages: b.messages, bounces: b.bounces}, nil
}

type session struct {
	resolver   *routing.Resolver
	messages   *messages.Service
	bounces    *bounce.Handler
	mailFrom   string
	recipients []routing.Result
	bounceRcpt []bounce.Recipient
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	s.mailFrom = from
	s.recipients = nil
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if recipient, bounceDomain, valid := s.bounces.ParseRecipient(to); bounceDomain {
		if !valid {
			return &smtp.SMTPError{Code: 550, Message: "invalid bounce recipient"}
		}
		s.bounceRcpt = append(s.bounceRcpt, recipient)
		return nil
	}

	result, ok, err := s.resolver.Resolve(context.Background(), to)
	if err != nil {
		return temporarySMTPError("temporary local problem")
	}
	if !ok {
		return &smtp.SMTPError{Code: 550, Message: "unknown recipient"}
	}
	for _, mbox := range result.Mailboxes {
		if routing.OverQuota(mbox) {
			return &smtp.SMTPError{Code: 552, Message: "mailbox over quota"}
		}
	}
	s.recipients = append(s.recipients, result)
	return nil
}

func (s *session) Data(r io.Reader) error {
	if len(s.recipients) == 0 && len(s.bounceRcpt) == 0 {
		return &smtp.SMTPError{Code: 554, Message: "no valid recipients"}
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return temporarySMTPError("temporary local problem")
	}
	for _, recipient := range s.bounceRcpt {
		if err := s.bounces.Record(context.Background(), recipient, raw); err != nil {
			slog.Error("bounce processing failed", "error", err)
			return temporarySMTPError("temporary local problem")
		}
	}
	if len(s.recipients) == 0 {
		return nil
	}
	if _, err := s.messages.Deliver(context.Background(), raw, s.recipients); err != nil {
		slog.Error("inbound delivery failed", "error", err)
		return temporarySMTPError("temporary local problem")
	}
	return nil
}

func (s *session) Reset() {
	s.mailFrom = ""
	s.recipients = nil
	s.bounceRcpt = nil
}

func (s *session) Logout() error {
	return nil
}

func temporarySMTPError(message string) *smtp.SMTPError {
	return &smtp.SMTPError{Code: 451, Message: message}
}
