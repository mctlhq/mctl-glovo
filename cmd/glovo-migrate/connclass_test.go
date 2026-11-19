package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Secrets that pgx may quote in an error. None may ever reach the class.
const leakyUser, leakyPass, leakyHost = "glovo_c2_migrator", "s3cr3t-pass-9", "db.internal.example"

func TestConnectClass(t *testing.T) {
	wrap := func(e error) error {
		return fmt.Errorf("failed to connect to `user=%s password=%s host=%s`: %w", leakyUser, leakyPass, leakyHost, e)
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"password refused", wrap(&pgconn.PgError{Code: "28P01", Message: "password authentication failed for user " + leakyUser}), "server refused, SQLSTATE 28P01"},
		{"database missing", wrap(&pgconn.PgError{Code: "3D000"}), "server refused, SQLSTATE 3D000"},
		{"hostile sqlstate", wrap(&pgconn.PgError{Code: "x'; DROP"}), "unclassified"},
		{"dns", wrap(&net.DNSError{Err: "no such host", Name: leakyHost}), "name resolution failed"},
		{"refused", wrap(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), "connection refused"},
		{"reset", wrap(&net.OpError{Op: "read", Err: syscall.ECONNRESET}), "connection reset"},
		{"unreachable", wrap(&net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}), "host unreachable"},
		{"deadline", wrap(context.DeadlineExceeded), "timed out"},
		{"unknown authority", wrap(x509.UnknownAuthorityError{}), "server certificate rejected"},
		{"hostname", wrap(x509.HostnameError{Host: leakyHost}), "server certificate rejected"},
		{"no tls", wrap(errors.New("server refused TLS connection")), "server does not offer TLS"},
		{"tls handshake", wrap(errors.New("tls: handshake failure")), "TLS handshake failed"},
		{"eof", wrap(errors.New("unexpected EOF")), "connection closed by the server"},
		{"op error other", wrap(&net.OpError{Op: "mystery " + leakyPass, Err: errors.New("x")}), "network error during connect"},
		{"anything else", wrap(errors.New("weird " + leakyPass)), "unclassified"},
	} {
		got := connectClass(tc.err)
		if got != tc.want {
			t.Errorf("%s: class %q, want %q", tc.name, got, tc.want)
		}
		for _, secret := range []string{leakyUser, leakyPass, leakyHost} {
			if strings.Contains(got, secret) {
				t.Errorf("%s: class leaks %q: %q", tc.name, secret, got)
			}
		}
	}
}

// End to end through migrate(): a refused connection names its class in the
// error and quotes nothing from the DSN.
func TestMigrateConnectFailureNamesTheClassNotTheDSN(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // nothing listens: connection refused
	dsn := "postgresql://" + leakyUser + ":" + leakyPass + "@" + addr + "/mctl-glovo?sslmode=require"
	_, err = migrate(context.Background(), config{DSN: dsn, ExpectRole: leakyUser, RuntimeRole: "glovo_c2_runtime"})
	if err == nil || exitCode(err) != exitFailed {
		t.Fatalf("want a failure (exit %d), got %v", exitFailed, err)
	}
	if !strings.Contains(err.Error(), "cannot connect to the database: connection refused") {
		t.Fatalf("error does not name the class: %q", err)
	}
	for _, secret := range []string{leakyPass, addr} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error quotes the DSN (%q): %q", secret, err)
		}
	}
}
