package inbound

import (
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/zephyraoss/haitatsu/internal/ratelimit"
)

const maxSessionDuration = 30 * time.Minute

type limitedListener struct {
	net.Listener
	perIP  *ratelimit.ConcurrencyGate
	global *ratelimit.ConcurrencyGate
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		key := connectionKey(conn.RemoteAddr())
		if !l.global.Acquire("") {
			refuse(conn, "421 4.7.0 too many connections\r\n")
			continue
		}
		if !l.perIP.Acquire(key) {
			l.global.Release("")
			refuse(conn, "421 4.7.0 too many connections from your address\r\n")
			continue
		}
		limited := &limitedConn{Conn: conn}
		limited.timer = time.AfterFunc(maxSessionDuration, func() { _ = conn.Close() })
		limited.release = func() {
			limited.timer.Stop()
			l.perIP.Release(key)
			l.global.Release("")
		}
		return limited, nil
	}
}

func refuse(conn net.Conn, reply string) {
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = conn.Write([]byte(reply))
	_ = conn.Close()
}

type limitedConn struct {
	net.Conn
	timer   *time.Timer
	release func()
	once    sync.Once
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

func connectionKey(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	ip = ip.Unmap()
	if ip.Is6() {
		prefix, _ := ip.Prefix(64)
		return prefix.String()
	}
	return ip.String()
}
