package main

import (
	"net/http"
	"testing"
)

// A ReadTimeout or WriteTimeout would cut long tool calls and the GET /mcp
// event stream (net/http cancels the request context when the read
// deadline fires), so the server must set neither.
func TestServerHasNoWholeRequestTimeouts(t *testing.T) {
	srv := newHTTPServer(http.NewServeMux())
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Fatalf("ReadTimeout=%v WriteTimeout=%v, want 0", srv.ReadTimeout, srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatal("ReadHeaderTimeout must bound slow headers")
	}
}
