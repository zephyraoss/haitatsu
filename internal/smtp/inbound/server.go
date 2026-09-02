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
	"github.com/zephyraoss/haitatsu/internal/mailstore"
	"github.com/zephyraoss/haitatsu/internal/messages"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/ratelimit"
	"github.com/zephyraoss/haitatsu/internal/routing"
	"github.com/zephyraoss/haitatsu/internal/spam"
)

type Server struct {
	server *smtp.Server
}

type Options struct {
	MaxMessageBytes     int64
	MaxRecipients       int
	MaxConnectionsPerIP int
	MessagesPerMinute   int
}

func New(cfg config.SMTPConfig, domain string, tlsConfig *tls.Config, resolver *routing.Resolver, messages *messages.Service, bounces *bounce.Handler, spamChecker *spam.Checker, m *metrics.Metrics, opts Options) *Server {
	backend := &backend{
		resolver: resolver,
		messages: messages,
		bounces:  bounces,
		spam:     spamChecker,
		metrics:  m,
		gate:     ratelimit.NewConcurrencyGate(opts.MaxConnectionsPerIP),
		limiter:  ratelimit.New(float64(opts.MessagesPerMinute)/60, opts.MessagesPerMinute),
	}
	server := smtp.NewServer(backend)
	server.Addr = cfg.InboundAddr
	server.Domain = domain
	server.TLSConfig = tlsConfig
	server.ReadTimeout = 60 * time.Second
	server.WriteTimeout = 60 * time.Second
	server.MaxMessageBytes = opts.MaxMessageBytes
	server.MaxRecipients = opts.MaxRecipients
	server.MaxLineLength = 4000
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

func (s *Server) Serve(listener net.Listener) error {
	err := s.server.Serve(listener)
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
	gate     *ratelimit.ConcurrencyGate
	limiter  *ratelimit.Limiter
}

func (b *backend) NewSession(conn *smtp.Conn) (smtp.Session, error) {
	ctx := smtpContext(conn)
	if !b.gate.Acquire(ctx.RemoteIP) {
		return nil, &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "too many connections from your address"}
	}
	b.metrics.SMTPConnection()
	return &session{backend: b, smtp: ctx}, nil
}

type session struct {
	backend    *backend
	smtp       spam.SMTPContext
	mailFrom   string
	recipients []routing.Result
	bounceRcpt []bounce.Recipient
	released   bool
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	if !s.backend.limiter.Allow(s.smtp.RemoteIP) {
		return &smtp.SMTPError{Code: 450, EnhancedCode: smtp.EnhancedCode{4, 7, 1}, Message: "rate limit exceeded, try again later"}
	}
	s.mailFrom = from
	s.smtp.MailFrom = from
	s.recipients = nil
	s.bounceRcpt = nil
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if recipient, bounceDomain, valid := s.backend.bounces.ParseRecipient(context.Background(), to); bounceDomain {
		if !valid {
			return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "invalid bounce recipient"}
		}
		s.bounceRcpt = append(s.bounceRcpt, recipient)
		return nil
	}

	result, ok, err := s.backend.resolver.Resolve(context.Background(), to)
	if err != nil {
		return temporarySMTPError("temporary local problem")
	}
	if !ok {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "unknown recipient"}
	}
	deliverable := result.Mailboxes[:0]
	for _, mbox := range result.Mailboxes {
		if mailstore.OverQuota(mbox) {
			s.backend.metrics.MailboxOverQuota()
			continue
		}
		deliverable = append(deliverable, mbox)
	}
	if len(deliverable) == 0 {
		return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 2, 2}, Message: "mailbox over quota"}
	}
	result.Mailboxes = deliverable
	s.recipients = append(s.recipients, result)
	return nil
}

func (s *session) Data(r io.Reader) error {
	if len(s.recipients) == 0 && len(s.bounceRcpt) == 0 {
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "no valid recipients"}
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return temporarySMTPError("temporary local problem")
	}
	for _, recipient := range s.bounceRcpt {
		if err := s.backend.bounces.Record(context.Background(), recipient, raw); err != nil {
			slog.Error("bounce processing failed", "error", err)
			return temporarySMTPError("temporary local problem")
		}
	}
	if len(s.recipients) == 0 {
		return nil
	}
	assessment := s.backend.spam.Check(context.Background(), raw, s.smtp, s.recipients)
	if assessment.Reject {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "message rejected by policy"}
	}
	if _, err := s.backend.messages.Deliver(context.Background(), raw, s.recipients, assessment); err != nil {
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
	if !s.released {
		s.released = true
		s.backend.gate.Release(s.smtp.RemoteIP)
	}
	return nil
}

func temporarySMTPError(message string) *smtp.SMTPError {
	return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: message}
}

func smtpContext(conn *smtp.Conn) spam.SMTPContext {
	remoteIP, _, _ := net.SplitHostPort(conn.Conn().RemoteAddr().String())
	_, tlsUsed := conn.TLSConnectionState()
	return spam.SMTPContext{RemoteIP: remoteIP, HELO: conn.Hostname(), TLS: tlsUsed}
}
