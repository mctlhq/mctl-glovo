package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// c2App.Shutdown waits for the keepalive, and reports it when the deadline
// passes first. A nil mcp is safe (the flag-off shape).
func TestC2ShutdownWaitsForKeepalive(t *testing.T) {
	t.Run("no keepalive", func(t *testing.T) {
		if err := (&c2App{}).Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("blocks until the keepalive is done", func(t *testing.T) {
		done := make(chan struct{})
		app := &c2App{keepaliveDone: done}
		returned := make(chan error, 1)
		go func() { returned <- app.Shutdown(context.Background()) }()
		select {
		case err := <-returned:
			t.Fatalf("Shutdown returned (%v) before the keepalive stopped", err)
		case <-time.After(100 * time.Millisecond):
		}
		close(done)
		select {
		case err := <-returned:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Shutdown did not return after the keepalive stopped")
		}
	})
	t.Run("deadline first", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		err := (&c2App{keepaliveDone: make(chan struct{})}).Shutdown(ctx)
		if err == nil || !strings.Contains(err.Error(), "keepalive did not stop in time") {
			t.Fatalf("got %v", err)
		}
	})
}
