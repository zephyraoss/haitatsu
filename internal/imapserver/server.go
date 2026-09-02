package imapserver

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/emersion/go-imap/v2"
	goimapserver "github.com/emersion/go-imap/v2/imapserver"

	passwordauth "github.com/zephyraoss/haitatsu/internal/auth"
	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/mailstore"
	"github.com/zephyraoss/haitatsu/internal/metrics"
	"github.com/zephyraoss/haitatsu/internal/ratelimit"
)

const labelPrefix = "Labels/"

type MessageStore interface {
	GetMessage(ctx context.Context, key string) ([]byte, error)
	PutMessage(ctx context.Context, key string, data []byte) error
}

type Server struct {
	addr   string
	server *goimapserver.Server
}

type Options struct {
	MaxConnectionsPerIP int
	AppendLimit         int64
}

func New(cfg config.IMAPConfig, tlsConfig *tls.Config, client *ent.Client, blobs MessageStore, store *mailstore.Store, m *metrics.Metrics, opts Options) *Server {
	throttle := passwordauth.NewFailureThrottle(10, 10*time.Minute, 15*time.Minute).WithStore(client)
	gate := ratelimit.NewConcurrencyGate(opts.MaxConnectionsPerIP)
	server := goimapserver.New(&goimapserver.Options{
		NewSession: func(conn *goimapserver.Conn) (goimapserver.Session, *goimapserver.GreetingData, error) {
			ip := remoteIP(conn.NetConn())
			if !gate.Acquire(ip) {
				return nil, nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "too many connections from your address"}
			}
			m.IMAPSessionStart()
			return &session{
				client:      client,
				blobs:       blobs,
				store:       store,
				metrics:     m,
				throttle:    throttle,
				gate:        gate,
				remoteIP:    ip,
				appendLimit: opts.AppendLimit,
			}, &goimapserver.GreetingData{}, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1:    {},
			imap.CapIdle:         {},
			imap.CapUIDPlus:      {},
			imap.CapMove:         {},
			imap.CapESearch:      {},
			imap.CapNamespace:    {},
			imap.CapUnselect:     {},
			imap.CapChildren:     {},
			imap.CapSpecialUse:   {},
			imap.CapListStatus:   {},
			imap.CapStatusSize:   {},
			imap.CapLiteralMinus: {},
			imap.CapSASLIR:       {},
		},
		TLSConfig:    tlsConfig,
		InsecureAuth: tlsConfig == nil,
	})
	return &Server{addr: cfg.Addr, server: server}
}

func (s *Server) Listen() error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	return s.server.Serve(listener)
}

func (s *Server) Serve(listener net.Listener) error {
	return s.server.Serve(listener)
}

func (s *Server) Shutdown(context.Context) error {
	return s.server.Close()
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
