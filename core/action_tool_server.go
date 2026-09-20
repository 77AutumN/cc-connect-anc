package core

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

// ListenActionToolsUnix exposes the same narrow /tool handler inside a file
// namespace. The caller verifies the parent ownership; existing socket paths
// are never removed or adopted. Session tokens remain mandatory.
func ListenActionToolsUnix(path string, handler http.Handler) (*ActionToolServer, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || handler == nil {
		return nil, errors.New("action tools require an absolute dedicated socket")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0022 != 0 || parent.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("action tools require a protected socket parent")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return nil, errors.New("action tool socket already exists or is unavailable")
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// The model uses a distinct UID. The protected mount and opaque token,
	// rather than broad filesystem access, authorize this HTTP endpoint.
	if err := os.Chmod(path, 0666); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return &ActionToolServer{listener: listener, server: &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 40 * time.Second,
		WriteTimeout: 40 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10,
	}}, nil
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
