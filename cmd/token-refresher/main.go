// Command token-refresher keeps a ZITADEL machine-user access token in a
// file, for a sidecar next to a service that re-reads the file per call.
// See internal/tokenrefresh for the behaviour and the failure policy.
//
// Environment: ZITADEL_ISSUER, ZITADEL_USER_ID, ZITADEL_KEY_ID,
// ZITADEL_SCOPE, KEY_FILE, TOKEN_FILE; optional TOKEN_FILE_MODE (octal,
// default 0644) and READY_FILE (exists while a valid token is on disk, for
// an exec probe).
//
// Exit codes: 0 stopped by signal; 1 token expired and could not be
// refreshed (file removed) or the file could not be managed; 2 refused
// configuration.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/mctlhq/mctl-glovo/internal/tokenrefresh"
)

func main() { os.Exit(realMain()) }

func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, os.Getenv, os.Stdout, os.Stderr)
}

func run(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer) int {
	cfg := tokenrefresh.Config{
		Issuer:    getenv("ZITADEL_ISSUER"),
		UserID:    getenv("ZITADEL_USER_ID"),
		KeyID:     getenv("ZITADEL_KEY_ID"),
		Scope:     getenv("ZITADEL_SCOPE"),
		KeyFile:   getenv("KEY_FILE"),
		TokenFile: getenv("TOKEN_FILE"),
		ReadyFile: getenv("READY_FILE"),
	}
	if m := getenv("TOKEN_FILE_MODE"); m != "" {
		v, err := strconv.ParseUint(m, 8, 32)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "token-refresher: TOKEN_FILE_MODE must be an octal mode")
			return 2
		}
		cfg.FileMode = os.FileMode(v) //nolint:gosec // bounded by ParseUint bitsize and validated in New
	}
	logger := slog.New(slog.NewJSONHandler(stdout, nil))
	r, err := tokenrefresh.New(cfg, tokenrefresh.Options{Logger: logger})
	if err != nil {
		var ce *tokenrefresh.ConfigError
		if errors.As(err, &ce) {
			_, _ = fmt.Fprintln(stderr, "token-refresher: "+ce.Error())
			return 2
		}
		_, _ = fmt.Fprintln(stderr, "token-refresher: startup failed")
		return 2
	}
	switch err := r.Run(ctx); {
	case errors.Is(err, context.Canceled):
		return 0
	case err != nil:
		_, _ = fmt.Fprintln(stderr, "token-refresher: "+err.Error())
		return 1
	}
	return 0
}
