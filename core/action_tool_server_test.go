package core

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestActionToolServerRefusesNonLoopbackAndOccupiedAddress(t *testing.T) {
	for _, address := range []string{"", ":18743", "0.0.0.0:18743", "localhost:18743", "[::1]:18743", "127.0.0.1:-1", "127.0.0.1:65536", "/tmp/tools.sock"} {
		if server, err := ListenActionTools(address, http.NotFoundHandler()); err == nil {
			_ = server.Close()
			t.Fatalf("accepted unsafe address %q", address)
		}
	}
	if server, err := ListenActionTools("127.0.0.1:0", nil); err == nil {
		_ = server.Close()
		t.Fatal("accepted a nil handler")
	}
	server, err := ListenActionTools("127.0.0.1:0", http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	address := server.listener.Addr().String()
	if second, err := ListenActionTools(address, http.NotFoundHandler()); err == nil {
		_ = second.Close()
		t.Fatal("occupied port silently changed or reused")
	}
	// Close before Serve must also release the port.
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	rebound, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	_ = rebound.Close()
}

func TestActionToolServerServesOnlyDedicatedHandlerAndCloses(t *testing.T) {
	e, _, _, _ := actionToolFixture(t)
	server, err := ListenActionTools("127.0.0.1:0", e.ActionToolHandler())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	address := server.listener.Addr().String()
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	for _, route := range []struct {
		path, token, command string
		status               int
	}{
		{"/tool", "test-session-token", "customer", http.StatusOK},
		{"/tool", "wrong-token", "customer", http.StatusUnauthorized},
		{"/cron/add", "test-session-token", "customer", http.StatusNotFound},
		{"/tool", "test-session-token", "approve", http.StatusBadRequest},
		{"/tool", "test-session-token", "apply", http.StatusBadRequest},
	} {
		req, _ := http.NewRequest(http.MethodPost, "http://"+address+route.path, strings.NewReader(`{"command":"`+route.command+`","input":{}}`))
		req.Header.Set("Authorization", "Bearer "+route.token)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != route.status {
			t.Fatalf("%s/%s returned %d, want %d", route.path, route.command, response.StatusCode, route.status)
		}
	}
	for range 2 {
		if err := server.Close(); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("unexpected serve exit: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not stop")
	}
	if conn, err := net.DialTimeout("tcp4", address, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("listener still accepts requests after Close")
	}
}
