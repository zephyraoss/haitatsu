package submission

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/mail"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	passwordauth "github.com/zephyraoss/haitatsu/internal/auth"
	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/apppassword"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailbox"
	"github.com/zephyraoss/haitatsu/internal/outbound"
)

type Server struct {
	startTLS *smtp.Server
	tls      *smtp.Server
}

func New(cfg config.SubmissionConfig, domain string, tlsConfig *tls.Config, client *ent.Client, submission *outbound.Submission) *Server {
	backend := &backend{client: client, submission: submission}
	var tlsServer *smtp.Server
	if tlsConfig != nil {
		tlsServer = newServer(cfg.TLSAddr, domain, tlsConfig, backend)
	}
	return &Server{
		startTLS: newServer(cfg.StartTLSAddr, domain, tlsConfig, backend),
		tls:      tlsServer,
	}
}

func (s *Server) Listen() error {
	errCh := make(chan error, 2)
	go func() { errCh <- s.startTLS.ListenAndServe() }()
	if s.tls != nil {
		go func() { errCh <- s.tls.ListenAndServeTLS() }()
	}
	return <-errCh
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

func newServer(addr string, domain string, tlsConfig *tls.Config, backend smtp.Backend) *smtp.Server {
	server := smtp.NewServer(backend)
	server.Addr = addr
	server.Domain = domain
	server.ReadTimeout = 10 * time.Second
	server.WriteTimeout = 10 * time.Second
	server.MaxMessageBytes = 50 * 1024 * 1024
	server.MaxRecipients = 100
	server.TLSConfig = tlsConfig
	server.AllowInsecureAuth = false
	return server
}

type backend struct {
	client     *ent.Client
	submission *outbound.Submission
}

func (b *backend) NewSession(*smtp.Conn) (smtp.Session, error) {
	return &session{client: b.client, submission: b.submission}, nil
}

type session struct {
	client     *ent.Client
	submission *outbound.Submission
	mailbox    *ent.Mailbox
	mailFrom   string
	recipients []string
}

func (s *session) AuthMechanisms() []string {
	return []string{"PLAIN"}
}

func (s *session) Auth(mech string) (sasl.Server, error) {
	if mech != "PLAIN" {
		return nil, smtp.ErrAuthUnknownMechanism
	}
	return sasl.NewPlainServer(s.authenticate), nil
}

func (s *session) authenticate(identity, username, password string) error {
	if identity != "" && identity != username {
		return smtp.ErrAuthFailed
	}
	mbox, err := s.client.Mailbox.Query().Where(mailbox.PrimaryAddressEqualFold(username), mailbox.StatusEQ("active"), mailbox.DeletedAtIsNil()).Only(context.Background())
	if err != nil {
		return smtp.ErrAuthFailed
	}
	passwords, err := s.client.AppPassword.Query().Where(apppassword.MailboxIDEQ(mbox.ID), apppassword.RevokedAtIsNil(), apppassword.DeletedAtIsNil()).All(context.Background())
	if err != nil {
		return err
	}
	for _, item := range passwords {
		valid, err := passwordauth.VerifyPassword(password, item.Hash)
		if err != nil {
			return err
		}
		if valid && hasScope(item.Scopes, "smtp") {
			s.mailbox = mbox
			_, _ = s.client.AppPassword.UpdateOneID(item.ID).SetLastUsedAt(time.Now()).Save(context.Background())
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
		return &smtp.SMTPError{Code: 553, Message: "invalid sender"}
	}
	if !strings.EqualFold(addr.Address, s.mailbox.PrimaryAddress) {
		return &smtp.SMTPError{Code: 553, Message: "sender not allowed"}
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
		return &smtp.SMTPError{Code: 553, Message: "invalid recipient"}
	}
	s.recipients = append(s.recipients, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	if s.mailbox == nil {
		return smtp.ErrAuthRequired
	}
	if s.mailFrom == "" || len(s.recipients) == 0 {
		return &smtp.SMTPError{Code: 554, Message: "sender and recipients required"}
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return temporaryError()
	}
	if _, err := s.submission.Submit(context.Background(), s.mailbox.ID, s.mailFrom, raw, s.recipients); err != nil {
		return submissionError(err)
	}
	return nil
}

func (s *session) Reset() {
	s.mailFrom = ""
	s.recipients = nil
}

func (s *session) Logout() error { return nil }

func submissionError(err error) error {
	if errors.Is(err, outbound.ErrSenderNotAllowed) {
		return &smtp.SMTPError{Code: 553, Message: "sender not allowed"}
	}
	if errors.Is(err, outbound.ErrDKIMKeyNotFound) {
		return &smtp.SMTPError{Code: 550, Message: "dkim key not configured for sender domain"}
	}
	return temporaryError()
}

func temporaryError() *smtp.SMTPError {
	return &smtp.SMTPError{Code: 451, Message: "temporary local problem"}
}

func hasScope(scopes []string, required string) bool {
	for _, scope := range scopes {
		if scope == required {
			return true
		}
	}
	return false
}
