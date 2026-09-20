//go:build windows

package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/platform"
)

func TestBootstrapNamedPipeReusedAndReleased(t *testing.T) {
	sid, err := platform.CurrentOwnerSID()
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("bootstrap-%d-%d", os.Getpid(), time.Now().UnixNano())
	listener, err := platform.ListenOwnerPipe(name, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var opened atomic.Int32
	closed := make(chan struct{}, 20)
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"version":"fixture"}`) }),
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				opened.Add(1)
			}
			if state == http.StateClosed {
				closed <- struct{}{}
			}
		},
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() {
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("owned server did not stop")
		}
	}()
	client := pipeClient(name)
	client.Timeout = 3 * time.Second
	defer client.CloseIdleConnections()
	handler := bootstrapHandler(client, "fixture")
	for i := 0; i < 20; i++ {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest("GET", "/api/v2/bootstrap", nil))
		if w.Code != 200 {
			t.Fatal("bootstrap failed", w.Code, w.Body.String())
		}
	}
	if opened.Load() != 1 {
		t.Fatal("named pipe was not reused", opened.Load())
	}
	client.CloseIdleConnections()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("desktop-owned idle pipe was not released")
	}
}
