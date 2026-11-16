// Command landing-css-sync vendors the pinned MCTL design-system sheets the
// landing page inlines (internal/gateway/landingassets/mctl.css, site.css).
//
//	go run ./cmd/landing-css-sync          # rewrite the vendored files
//	go run ./cmd/landing-css-sync -check   # fail if they differ from the pin
//
// The source is the immutable ui.mctl.ai/<version>/ path named by
// gateway.LandingDesignVersion; the floating path is never used.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/gateway"
)

var sheets = []string{"mctl", "site"}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func wanted(ctx context.Context, sheet string) ([]byte, error) {
	body, err := fetch(ctx, gateway.LandingDesignSource(sheet))
	if err != nil {
		return nil, err
	}
	return append([]byte(gateway.LandingVendoredHeader(sheet)), body...), nil
}

func main() {
	check := flag.Bool("check", false, "report drift instead of writing")
	dir := flag.String("dir", "internal/gateway/landingassets", "vendored sheet directory")
	flag.Parse()
	ctx := context.Background()
	stale := false
	for _, s := range sheets {
		want, err := wanted(ctx, s)
		if err != nil {
			fmt.Fprintln(os.Stderr, "landing-css-sync:", err)
			os.Exit(1)
		}
		path := filepath.Join(*dir, s+".css")
		if !*check {
			if err := os.WriteFile(path, want, 0o644); err != nil { //nolint:gosec // G306: public vendored source
				fmt.Fprintln(os.Stderr, "landing-css-sync:", err)
				os.Exit(1)
			}
			fmt.Println("wrote", path, "from", gateway.LandingDesignSource(s))
			continue
		}
		got, err := os.ReadFile(path) //nolint:gosec // G304: fixed repo path from a flag default
		if err != nil || !bytes.Equal(got, want) {
			fmt.Fprintf(os.Stderr, "%s does not match %s: run `go run ./cmd/landing-css-sync`, review the diff and commit it\n", path, gateway.LandingDesignSource(s))
			stale = true
			continue
		}
		fmt.Println(path, "matches", gateway.LandingDesignSource(s))
	}
	if stale {
		os.Exit(1)
	}
}
