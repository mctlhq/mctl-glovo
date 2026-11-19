package main

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
)

// connectClass names WHY a connection attempt failed, from a fixed vocabulary.
// pgx's own message can quote the DSN (user, password, host), so it is never
// printed; the class is chosen from the error's type or from fixed substrings
// of its text and is itself a constant. A server error contributes only its
// SQLSTATE code (five characters of [0-9A-Z]), which names the failure without
// carrying anything from the request.
func connectClass(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && sqlstateShape(pe.Code) {
		return "server refused, SQLSTATE " + pe.Code
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "name resolution failed"
	}
	var unknownAuthority x509.UnknownAuthorityError
	var certInvalid x509.CertificateInvalidError
	var hostErr x509.HostnameError
	if errors.As(err, &unknownAuthority) || errors.As(err, &certInvalid) || errors.As(err, &hostErr) {
		return "server certificate rejected"
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "host unreachable"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "connection closed by the server"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timed out"
	}
	// The remaining classes are read from fixed fragments of the text of the
	// INNERMOST error only: the outer text is built from the DSN (user, host),
	// and a host called "eof-db" must not read as a closed connection. The text is
	// only inspected, never echoed.
	msg := strings.ToLower(innermost(err).Error())
	switch {
	case strings.Contains(msg, "server refused tls"), strings.Contains(msg, "does not support ssl"):
		return "server does not offer TLS"
	case strings.HasPrefix(msg, "tls:"):
		return "TLS handshake failed"
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return "network error during " + opName(op.Op)
	}
	return "unclassified"
}

// innermost follows Unwrap to the error at the bottom of the chain.
func innermost(err error) error {
	for {
		u := errors.Unwrap(err)
		if u == nil {
			return err
		}
		err = u
	}
}

// opName keeps only a known net.OpError operation name.
func opName(op string) string {
	switch op {
	case "dial", "read", "write":
		return op
	}
	return "connect"
}

func sqlstateShape(c string) bool {
	if len(c) != 5 {
		return false
	}
	for _, r := range c {
		if (r < '0' || r > '9') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}
