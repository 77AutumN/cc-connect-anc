package core

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ActionToolServer owns only a dedicated IPv4 loopback HTTP listener. It never
// registers privileged daemon API routes or starts another process/service.
type ActionToolServer struct {
	server    *http.Server
	listener  net.Listener
	closeOnce sync.Once
	closeErr  error
}

// ListenActionTools requires a literal loopback address and never falls back to
// another interface/port. Production pins the port; tests may request port zero.
func ListenActionTools(address string, handler http.Handler) (*ActionToolServer, error) {
	host, port, err := net.SplitHostPort(address)
	number, portErr := strconv.Atoi(port)
	if err != nil || host != "127.0.0.1" || portErr != nil || number < 0 || number > 65535 || handler == nil {
		return nil, errors.New("action tools require an IPv4 loopback address and handler")
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return nil, fmt.Errorf("listen on action tool loopback: %w", err)
	}
	return &ActionToolServer{listener: listener, server: &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 35 * time.Second, WriteTimeout: 35 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10,
	}}, nil
}

func (s *ActionToolServer) Serve() error { return s.server.Serve(s.listener) }

// Close is idempotent, including before Serve. Interrupted requests remain
// governed by the ledger; shutdown does not retry or roll back business writes.
func (s *ActionToolServer) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.server.Close()
		if err := s.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			s.closeErr = errors.Join(s.closeErr, err)
		}
	})
	return s.closeErr
}
