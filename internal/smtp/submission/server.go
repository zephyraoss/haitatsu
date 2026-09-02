package submission

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/mail"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	passwordauth "github.com/zephyraoss/haitatsu/internal/auth"
	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/apppassword"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailbox"
	"github.com/zephyraoss/haitatsu/internal/outbound"
	"github.com/zephyraoss/haitatsu/internal/ratelimit"
)

type Server struct {
	startTLS *smtp.Server
	tls      *smtp.Server
}

type Options struct {
	MaxMessageBytes     int64
	MaxRecipients       int
	MaxConnectionsPerIP int
}

func New(cfg config.SubmissionConfig, domain string, tlsConfig *tls.Config, client *ent.Client, submission *outbound.Submission, opts Options) *Server {
	backend := &backend{
		client:     client,
		submission: submission,
		throttle:   passwordauth.NewFailureThrottle(10, 10*time.Minute, 15*time.Minute).WithStore(client),
		gate:       ratelimit.NewConcurrencyGate(opts.MaxConnectionsPerIP),
	}
	var tlsServer *smtp.Server
	if tlsConfig != nil {
		tlsServer = newServer(cfg.TLSAddr, domain, tlsConfig, backend, opts)
	}
	return &Server{
		startTLS: newServer(cfg.StartTLSAddr, domain, tlsConfig, backend, opts),
		tls:      tlsServer,
	}
}

func (s *Server) Listen() error {
	errCh := make(chan error, 2)
	go func() { errCh <- ignoreClosed(s.startTLS.ListenAndServe()) }()
	if s.tls != nil {
		go func() { errCh <- ignoreClosed(s.tls.ListenAndServeTLS()) }()
	}
	return <-errCh
}

func (s *Server) Serve(listener net.Listener) error {
	return ignoreClosed(s.startTLS.Serve(listener))
}

func ignoreClosed(err error) error {
	if errors.Is(err, smtp.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.startTLS.Shutdown(ctx); err != nil {
		return err
	}
	if s.tls == nil {
		return nil
	}
	return s.tls.Shutdown(ctx)
}

func newServer(addr string, domain string, tlsConfig *tls.Config, backend smtp.Backend, opts Options) *smtp.Server {
	server := smtp.NewServer(backend)
	server.Addr = addr
	server.Domain = domain
	server.ReadTimeout = 60 * time.Second
	server.WriteTimeout = 60 * time.Second
	server.MaxMessageBytes = opts.MaxMessageBytes
	server.MaxRecipients = opts.MaxRecipients
	server.MaxLineLength = 4000
	server.TLSConfig = tlsConfig
	server.AllowInsecureAuth = tlsConfig == nil
	return server
}

type backend struct {
	client     *ent.Client
	submission *outbound.Submission
	throttle   *passwordauth.FailureThrottle
	gate       *ratelimit.ConcurrencyGate
}

func (b *backend) NewSession(conn *smtp.Conn) (smtp.Session, error) {
	ip := remoteIP(conn.Conn())
	if !b.gate.Acquire(ip) {
		return nil, &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "too many connections from your address"}
	}
	return &session{backend: b, remoteIP: ip}, nil
}

type session struct {
	backend    *backend
	remoteIP   string
	mailbox    *ent.Mailbox
	mailFrom   string
	recipients []string
	released   bool
}

func remoteIP(conn net.Conn) string {
	if conn == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return conn.RemoteAddr().String()
	}
	return host
}

func (s *session) AuthMechanisms() []string {
	return []string{"PLAIN", "LOGIN"}
}

func (s *session) Auth(mech string) (sasl.Server, error) {
	switch mech {
	case "PLAIN":
		return sasl.NewPlainServer(s.authenticate), nil
	case "LOGIN":
		return newLoginServer(func(username, password string) error { return s.authenticate("", username, password) }), nil
	}
	return nil, smtp.ErrAuthUnknownMechanism
}

func (s *session) authenticate(identity, username, password string) error {
	if s.backend.throttle.Blocked(s.remoteIP) {
		return smtp.ErrAuthFailed
	}
	if err := s.verifyCredentials(identity, username, password); err != nil {
		s.backend.throttle.RecordFailure(s.remoteIP)
		return err
	}
	s.backend.throttle.RecordSuccess(s.remoteIP)
	return nil
}

func (s *session) verifyCredentials(identity, username, password string) error {
	if identity != "" && identity != username {
		return smtp.ErrAuthFailed
	}
	ctx := context.Background()
	mbox, err := s.backend.client.Mailbox.Query().Where(mailbox.PrimaryAddressEqualFold(username), mailbox.StatusEQ("active"), mailbox.DeletedAtIsNil()).Only(ctx)
	if err != nil {
		return smtp.ErrAuthFailed
	}
	passwords, err := s.backend.client.AppPassword.Query().Where(apppassword.MailboxIDEQ(mbox.ID), apppassword.RevokedAtIsNil(), apppassword.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return err
	}
	for _, item := range passwords {
		valid, err := passwordauth.VerifyPassword(password, item.Hash)
		if err != nil {
			return err
		}
		if valid && passwordauth.HasScope(item.Scopes, "smtp") {
			s.mailbox = mbox
			_, _ = s.backend.client.AppPassword.UpdateOneID(item.ID).SetLastUsedAt(time.Now()).Save(ctx)
			return nil
		}
	}
	return smtp.ErrAuthFailed
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	if s.mailbox == nil {
		return smtp.ErrAuthRequired
	}
	addr, err := mail.ParseAddress(from)
	if err != nil {
		return &smtp.SMTPError{Code: 553, EnhancedCode: smtp.EnhancedCode{5, 1, 7}, Message: "invalid sender"}
	}
	allowed, err := s.backend.submission.SenderAllowed(context.Background(), s.mailbox, addr.Address)
	if err != nil {
		return temporaryError()
	}
	if !allowed {
		return &smtp.SMTPError{Code: 553, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "sender not allowed"}
	}
	s.mailFrom = addr.Address
	s.recipients = nil
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if s.mailbox == nil {
		return smtp.ErrAuthRequired
	}
	if _, err := mail.ParseAddress(to); err != nil {
		return &smtp.SMTPError{Code: 553, EnhancedCode: smtp.EnhancedCode{5, 1, 3}, Message: "invalid recipient"}
	}
	s.recipients = append(s.recipients, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	if s.mailbox == nil {
		return smtp.ErrAuthRequired
	}
	if s.mailFrom == "" || len(s.recipients) == 0 {
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "sender and recipients required"}
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return temporaryError()
	}
	if _, err := s.backend.submission.Submit(context.Background(), s.mailbox.ID, s.mailFrom, raw, s.recipients); err != nil {
		return submissionError(err)
	}
	return nil
}

func (s *session) Reset() {
	s.mailFrom = ""
	s.recipients = nil
}

func (s *session) Logout() error {
	if !s.released {
		s.released = true
		s.backend.gate.Release(s.remoteIP)
	}
	return nil
}

func submissionError(err error) error {
	switch {
	case errors.Is(err, outbound.ErrSenderNotAllowed):
		return &smtp.SMTPError{Code: 553, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "sender not allowed"}
	case errors.Is(err, outbound.ErrDKIMKeyNotFound):
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "dkim key not configured for sender domain"}
	case errors.Is(err, outbound.ErrRateLimited):
		return &smtp.SMTPError{Code: 450, EnhancedCode: smtp.EnhancedCode{4, 7, 1}, Message: "outbound rate limit exceeded"}
	case errors.Is(err, outbound.ErrTooManyRecipients):
		return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 5, 3}, Message: "too many recipients"}
	case errors.Is(err, outbound.ErrOverQuota):
		return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 2, 2}, Message: "mailbox over quota"}
	}
	return temporaryError()
}

func temporaryError() *smtp.SMTPError {
	return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "temporary local problem"}
}
