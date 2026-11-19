package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func refused() error {
	return fmt.Errorf("dial: %w", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED})
}

// no sleeping in tests: the helper only needs to be told how often it slept.
type fakeSleep struct{ n int }

func (f *fakeSleep) sleep(_ context.Context, _ time.Duration) { f.n++ }

// The live failure: the first attempts are refused, a later one works.
func TestRetryConnectSucceedsAfterTransientRefusals(t *testing.T) {
	calls := 0
	fs := &fakeSleep{}
	n, err := retryConnect(context.Background(), func(context.Context) error {
		calls++
		if calls <= 3 {
			return refused()
		}
		return nil
	}, time.Hour, fs.sleep)
	if err != nil || n != 4 || fs.n != 3 {
		t.Fatalf("attempts %d, sleeps %d, err %v; want 4, 3, nil", n, fs.n, err)
	}
}

// A real answer is not waited out: a refused login, a bad certificate, a
// server without TLS and anything unclassified end the loop on the first try.
func TestRetryConnectDoesNotRetryARealAnswer(t *testing.T) {
	for name, e := range map[string]error{
		"password refused": &pgconn.PgError{Code: "28P01"},
		"database missing": &pgconn.PgError{Code: "3D000"},
		"no tls":           errors.New("server refused TLS connection"),
		"tls handshake":    errors.New("tls: handshake failure"),
		"unclassified":     errors.New("something odd"),
	} {
		fs := &fakeSleep{}
		n, err := retryConnect(context.Background(), func(context.Context) error { return e }, time.Hour, fs.sleep)
		if n != 1 || fs.n != 0 || !errors.Is(err, e) {
			t.Errorf("%s: attempts %d, sleeps %d, err %v; want exactly one attempt and the same error", name, n, fs.n, err)
		}
	}
}

// The wait is bounded: a refusal that never clears ends with the last error.
func TestRetryConnectGivesUpAfterTheWait(t *testing.T) {
	start := time.Now()
	n, err := retryConnect(context.Background(), func(context.Context) error { return refused() }, 700*time.Millisecond, sleepCtx)
	if err == nil || n < 2 {
		t.Fatalf("attempts %d, err %v; want several attempts and an error", n, err)
	}
	if connectClass(err) != "connection refused" {
		t.Fatalf("last error class %q", connectClass(err))
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("took %v for a 700ms wait", el)
	}
}

// A negative wait is one attempt (no retry), and the context always wins.
func TestRetryConnectNegativeWaitAndContext(t *testing.T) {
	fs := &fakeSleep{}
	if n, err := retryConnect(context.Background(), func(context.Context) error { return refused() }, -1, fs.sleep); n != 1 || err == nil || fs.n != 0 {
		t.Fatalf("negative wait: attempts %d, sleeps %d, err %v", n, fs.n, err)
	}
	// A context that ends during the attempt stops the loop before any sleep.
	ctx, cancel := context.WithCancel(context.Background())
	fs2 := &fakeSleep{}
	if n, err := retryConnect(ctx, func(context.Context) error { cancel(); return refused() }, time.Hour, fs2.sleep); n != 1 || err == nil || fs2.n != 0 {
		t.Fatalf("context ended during the attempt: attempts %d, sleeps %d, err %v", n, fs2.n, err)
	}
}

func TestIsTransient(t *testing.T) {
	for class, want := range map[string]bool{
		"connection refused":             true,
		"timed out":                      true,
		"name resolution failed":         true,
		"server refused, SQLSTATE 28P01": false,
		"server certificate rejected":    false,
		"server does not offer TLS":      false,
		"TLS handshake failed":           false,
		"unclassified":                   false,
		"":                               false,
	} {
		if got := isTransient(class); got != want {
			t.Errorf("isTransient(%q) = %v, want %v", class, got, want)
		}
	}
	if strings.Contains(connectClass(refused()), "SQLSTATE") {
		t.Fatal("refused classed as a server answer")
	}
}
