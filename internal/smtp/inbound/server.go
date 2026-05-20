package inbound

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/zephyraoss/haitatsu/internal/bounce"
	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/messages"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/routing"
	"github.com/zephyraoss/haitatsu/internal/spam"
)

type Server struct {
	server *smtp.Server
}

func New(cfg config.SMTPConfig, domain string, tlsConfig *tls.Config, resolver *routing.Resolver, messages *messages.Service, bounces *bounce.Handler, spamChecker *spam.Checker, metrics *metrics.Metrics) *Server {
	server := smtp.NewServer(&backend{resolver: resolver, messages: messages, bounces: bounces, spam: spamChecker, metrics: metrics})
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
	spam     *spam.Checker
	metrics  *metrics.Metrics
}

func (b *backend) NewSession(conn *smtp.Conn) (smtp.Session, error) {
	b.metrics.SMTPConnection()
	return &session{resolver: b.resolver, messages: b.messages, bounces: b.bounces, spam: b.spam, metrics: b.metrics, smtp: smtpContext(conn)}, nil
}

type session struct {
	resolver   *routing.Resolver
	messages   *messages.Service
	bounces    *bounce.Handler
	spam       *spam.Checker
	metrics    *metrics.Metrics
	smtp       spam.SMTPContext
	mailFrom   string
	recipients []routing.Result
	bounceRcpt []bounce.Recipient
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	s.mailFrom = from
	s.smtp.MailFrom = from
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
			s.metrics.MailboxOverQuota()
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
	assessment := s.spam.Check(context.Background(), raw, s.smtp, s.recipients)
	if assessment.Reject {
		return &smtp.SMTPError{Code: 550, Message: "message rejected by policy"}
	}
	if _, err := s.messages.Deliver(context.Background(), raw, s.recipients, assessment); err != nil {
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

func smtpContext(conn *smtp.Conn) spam.SMTPContext {
	remoteIP, _, _ := net.SplitHostPort(conn.Conn().RemoteAddr().String())
	_, tlsUsed := conn.TLSConnectionState()
	return spam.SMTPContext{RemoteIP: remoteIP, HELO: conn.Hostname(), TLS: tlsUsed}
}
