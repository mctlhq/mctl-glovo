// Command glovo-migrate applies the token store schema (internal/tokenstore
// migrations) to the C2 database, as the migration owner role. It is the
// entrypoint of the migration Job; the runtime service never migrates.
//
// It is safe to run on every deploy:
//   - empty database: the schema is applied in one transaction and the
//     store ships killed (glovo_control.c2_killed = true);
//   - schema already at the version this build expects: nothing is changed,
//     in particular the kill switch is not reset;
//   - anything else (a different version, tables without a version row, an
//     unreadable catalog) is refused or reported as a failure, never
//     "fixed". Could not observe is never treated as absent.
//
// The connection string comes from the environment variable named by
// -dsn-env and is never printed. Exit codes: 0 ok (applied or already
// current); 1 refused (bad flags, no TLS, wrong role, unexpected schema);
// 3 could not observe the database or a step failed.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"
)

const (
	exitOK      = 0
	exitRefused = 1
	exitFailed  = 3
)

func main() { os.Exit(realMain()) }

func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("glovo-migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dsnEnv := fs.String("dsn-env", "GLOVO_MIGRATE_DATABASE_URL", "name of the environment variable that holds the migrator DSN")
	expect := fs.String("expect-role", "glovo_c2_migrator", "the role the connection must be")
	runtime := fs.String("runtime-role", "", "role that receives the runtime grants (empty: grant nothing)")
	noTLS := fs.Bool("insecure-no-tls", false, "allow a connection without TLS (local development and CI only)")
	timeout := fs.Duration("timeout", 5*time.Minute, "overall time limit")
	if err := fs.Parse(args); err != nil {
		return exitRefused
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "glovo-migrate: unexpected arguments")
		return exitRefused
	}
	dsn := getenv(*dsnEnv)
	if dsn == "" {
		_, _ = fmt.Fprintf(stderr, "glovo-migrate: the environment variable %s is empty\n", *dsnEnv)
		return exitRefused
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	res, err := migrate(ctx, config{DSN: dsn, ExpectRole: *expect, RuntimeRole: *runtime, AllowNoTLS: *noTLS})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "glovo-migrate:", err)
		return exitCode(err)
	}
	_, _ = fmt.Fprintf(stdout, "glovo-migrate: schema %s at version %d\n", res.Outcome, res.Version)
	if res.GrantsApplied {
		_, _ = fmt.Fprintln(stdout, "glovo-migrate: runtime grants applied")
	}
	return exitOK
}
