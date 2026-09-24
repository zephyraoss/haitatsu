package outbound

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/zephyraoss/haitatsu/internal/config"
)

func stallingSMTPServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var buf [1]byte
				for {
					if _, err := conn.Read(buf[:]); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func TestSendViaRelayTimesOutOnStalledGreeting(t *testing.T) {
	addr := stallingSMTPServer(t)
	cfg := config.RelayConfig{Addr: addr, TimeoutSeconds: 1}
	start := time.Now()
	err := sendViaRelay(context.Background(), cfg, "a@example.com", []string{"b@example.com"}, []byte("Subject: hi\r\n\r\nbody\r\n"))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("send took %v, expected to abort near the 1s timeout", elapsed)
	}
}

func TestSendViaRelayHonorsCancelledContext(t *testing.T) {
	addr := stallingSMTPServer(t)
	cfg := config.RelayConfig{Addr: addr}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := sendViaRelay(ctx, cfg, "a@example.com", []string{"b@example.com"}, []byte("body"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("send took %v after cancel", elapsed)
	}
}
