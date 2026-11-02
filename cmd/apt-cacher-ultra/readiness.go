package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// readiness is monotonic across startup and shutdown. Each configured HTTP
// server signals once on entering Accept, after its own synchronous setup.
type readiness struct {
	ctx       context.Context
	required  int32
	accepting atomic.Int32
	stopped   atomic.Bool
	certs     *serverTLS
	mitm      *tlsMitmHandles
}

func (s *readiness) failure() string {
	if s.ctx.Err() != nil || s.stopped.Load() || (s.certs.ctx != nil && s.certs.ctx.Err() != nil) {
		return "shutdown"
	}
	if s.accepting.Load() != s.required {
		return "startup"
	}
	if cert, ok := s.certs.Snapshots()["cache"]; ok && cert.State != "usable" && cert.State != "degraded" {
		return "proxy_tls"
	}
	if s.mitm != nil {
		now := time.Now().Unix()
		if now < s.mitm.CANotBeforeUnixTime || now >= s.mitm.CANotAfterUnixTime {
			return "mitm_ca"
		}
	}
	return ""
}

func (s *readiness) listener(ln net.Listener) net.Listener {
	return &readyListener{Listener: ln, state: s}
}

type readyListener struct {
	net.Listener
	state *readiness
	once  sync.Once
}

func (l *readyListener) Accept() (net.Conn, error) {
	l.once.Do(func() { l.state.accepting.Add(1) })
	conn, err := l.Listener.Accept()
	if err != nil {
		var temporary interface{ Temporary() bool }
		if !errors.As(err, &temporary) || !temporary.Temporary() {
			l.state.stopped.Store(true)
		}
	}
	return conn, err
}

func listenerAddress(ln net.Listener, configured string) string {
	if ln != nil {
		return ln.Addr().String()
	}
	return configured
}

// bindApplicationListeners runs only after all fallible preparation. On a
// partial bind failure its caller closes every socket before cleaning up TLS.
func bindApplicationListeners(ctx context.Context, plain, secure, admin *net.Listener, httpAddr, tlsAddr, adminAddr string) error {
	for _, item := range []struct {
		target *net.Listener
		addr   string
	}{
		{plain, httpAddr}, {secure, tlsAddr}, {admin, adminAddr},
	} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if *item.target != nil || item.addr == "" {
			continue
		}
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", item.addr)
		if err != nil {
			return err
		}
		*item.target = ln
	}
	return ctx.Err()
}
