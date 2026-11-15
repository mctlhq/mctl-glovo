package glovoapi

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// rawHead is one request as it crossed the socket.
type rawHead struct {
	line    string
	headers [][2]string // exact names and values, in wire order
	body    string
}

// rawGlovo is a plain TCP listener that records request heads byte for
// byte, so header spelling and anything net/http adds are visible.
type rawGlovo struct {
	ln    net.Listener
	mu    sync.Mutex
	heads []rawHead
}

func newRawGlovo(t *testing.T, reply func(path string) string) *rawGlovo {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &rawGlovo{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go g.serve(conn, reply)
		}
	}()
	return g
}

func (g *rawGlovo) serve(conn net.Conn, reply func(string) string) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		h := rawHead{line: strings.TrimRight(line, "\r\n")}
		n := 0
		for {
			l, err := br.ReadString('\n')
			if err != nil {
				return
			}
			l = strings.TrimRight(l, "\r\n")
			if l == "" {
				break
			}
			k, v, _ := strings.Cut(l, ":")
			v = strings.TrimSpace(v)
			h.headers = append(h.headers, [2]string{k, v})
			if strings.EqualFold(k, "Content-Length") {
				n, _ = strconv.Atoi(v)
			}
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(br, body); err != nil {
			return
		}
		h.body = string(body)
		g.mu.Lock()
		g.heads = append(g.heads, h)
		g.mu.Unlock()
		out := reply(strings.Fields(h.line)[1])
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(out), out)
	}
}

func (g *rawGlovo) all() []rawHead {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]rawHead(nil), g.heads...)
}

// upstreamNames are glovo-skill 0.2.1's baseHeaders names, spelled as it
// passes them to fetch; undici sends them in that spelling.
var upstreamNames = []string{
	"Accept", "Content-Type", "Glovo-Api-Version", "Glovo-App-Context", "Glovo-App-Development-State",
	"Glovo-App-Platform", "Glovo-App-Type", "Glovo-App-Version", "Glovo-Client-Info", "Glovo-Device-Urn",
	"Glovo-Language-Code", "Glovo-Location-Country-Code", "Glovo-Location-City-Code",
	"Glovo-Delivery-Location-Latitude", "Glovo-Delivery-Location-Longitude", "Glovo-Delivery-Location-Timestamp",
	"Glovo-Delivery-Location-Accuracy", "Glovo-Perseus-Client-Id", "Glovo-Perseus-Session-Id",
	"Glovo-Perseus-Session-Timestamp", "Glovo-Perseus-Consent", "Glovo-Request-Id", "Glovo-Request-TTL",
}

// The token refresh and the authenticated call that follows it carry
// exactly glovo-skill's headers plus undici's, over HTTP/1.1, through the
// client's default transport: no "Go-http-client" agent, no second
// Accept-Encoding, nothing invented.
func TestWireRequestMatchesUpstream(t *testing.T) {
	g := newRawGlovo(t, func(p string) string {
		if p == "/oauth/refresh" {
			return `{"accessToken":"` + validToken + `","refreshToken":"refresh-test-new","expiresIn":3600}`
		}
		return `[]`
	})
	s := testSession()
	s["accessToken"] = expiredToken
	ids := 0
	c, err := New(writeSessionFile(t, s),
		WithBaseURL("http://"+g.ln.Addr().String()),
		WithDefaultLocation(testLocation()),
		WithClock(func() time.Time { return testNow }),
		WithIDGenerator(func() string { ids++; return fmt.Sprintf("00000000-0000-4000-8000-%012d", ids) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetSavedLocations(context.Background()); err != nil {
		t.Fatal(err)
	}
	heads := g.all()
	if len(heads) != 2 || heads[0].line != "POST /oauth/refresh HTTP/1.1" || !strings.HasSuffix(heads[1].line, " HTTP/1.1") {
		t.Fatalf("requests: %+v", heads)
	}
	if heads[0].body != `{"refreshToken":"refresh-test-old"}` {
		t.Errorf("refresh body = %s", heads[0].body)
	}
	for i, h := range heads {
		var names []string
		values := map[string][]string{}
		for _, kv := range h.headers {
			names = append(names, kv[0])
			values[strings.ToLower(kv[0])] = append(values[strings.ToLower(kv[0])], kv[1])
		}
		want := append(append([]string{"Host"}, upstreamNames...), "accept-language", "sec-fetch-mode", "User-Agent", "Accept-Encoding")
		if i == 0 {
			want = append(want, "Content-Length")
		} else {
			want = append(want, "Authorization")
		}
		slices.Sort(names)
		slices.Sort(want)
		if !slices.Equal(names, want) {
			t.Errorf("%s: header names\n got %v\nwant %v", h.line, names, want)
		}
		for k, v := range map[string]string{
			"user-agent": "node", "accept-encoding": "gzip, deflate", "accept-language": "*", "sec-fetch-mode": "cors",
			"glovo-app-context": "web", "glovo-app-platform": "web", "glovo-app-version": "v1.2476.1",
			"glovo-client-info": "web-customer-web-react/v1.2476.1 project:customer-web",
		} {
			if got := values[k]; len(got) != 1 || got[0] != v {
				t.Errorf("%s: %s = %q, want %q", h.line, k, got, v)
			}
		}
		if _, ok := values["authorization"]; ok == (i == 0) {
			t.Errorf("%s: Authorization present = %v", h.line, ok)
		}
	}
}

// A location may carry its own client identity, as upstream's
// location.appVersion and location.clientInfo.
func TestLocationOverridesClientIdentity(t *testing.T) {
	f := newFake(t)
	s := testSession()
	s["location"].(map[string]any)["appVersion"] = "v9.9.9"
	s["location"].(map[string]any)["clientInfo"] = "client-info-test"
	c, _ := newTestClient(t, f, s)
	if _, err := c.GetStore(context.Background(), "example-market"); err != nil {
		t.Fatal(err)
	}
	h := f.last().Header
	if h.Get("Glovo-App-Version") != "v9.9.9" || h.Get("Glovo-Client-Info") != "client-info-test" {
		t.Errorf("identity: %q %q", h.Get("Glovo-App-Version"), h.Get("Glovo-Client-Info"))
	}
}

// Node's fetch speaks HTTP/1.1; the default transport must not upgrade to
// HTTP/2 even when the server offers it. The control shows the server does.
func TestDefaultTransportIsHTTP1(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, map[int]string{1: "HTTP/1.1", 2: "HTTP/2.0"}[r.ProtoMajor])
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	get := func(rt http.RoundTripper) string {
		resp, err := (&http.Client{Transport: rt}).Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if got := get(srv.Client().Transport); got != "HTTP/2.0" {
		t.Fatalf("control: server negotiated %s, want HTTP/2.0", got)
	}
	tr, err := newTransport()
	if err != nil {
		t.Fatal(err)
	}
	// Trust the test CA, and offer h2 as a transport that already spoke
	// HTTP/2 would: the client must still not negotiate it.
	tr.TLSClientConfig.RootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	if !slices.Equal(tr.TLSClientConfig.NextProtos, []string{"http/1.1"}) {
		t.Errorf("ALPN = %v, want only http/1.1", tr.TLSClientConfig.NextProtos)
	}
	poisoned := tr.TLSClientConfig.Clone()
	poisoned.NextProtos = []string{"h2", "http/1.1"}
	tr.TLSClientConfig = http1Only(poisoned)
	if got := get(tr); got != "HTTP/1.1" {
		t.Errorf("default transport negotiated %s, want HTTP/1.1", got)
	}
	c, err := New("unused")
	if err != nil {
		t.Fatal(err)
	}
	if ct, ok := c.httpClient.Transport.(*http.Transport); !ok || ct.ForceAttemptHTTP2 || ct.TLSNextProto == nil || !ct.DisableCompression {
		t.Errorf("New's transport: %#v", c.httpClient.Transport)
	}
}

// The request asks for "gzip, deflate" as undici does, so the client must
// decode both itself, and refuse an encoding it does not know.
func TestResponseContentEncoding(t *testing.T) {
	const doc = `{"ok":true}`
	var gz, zl, raw bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write([]byte(doc))
	_ = gw.Close()
	zw := zlib.NewWriter(&zl)
	_, _ = zw.Write([]byte(doc))
	_ = zw.Close()
	fw, _ := flate.NewWriter(&raw, flate.DefaultCompression)
	_, _ = fw.Write([]byte(doc))
	_ = fw.Close()
	for _, tc := range []struct {
		enc  string
		body []byte
		ok   bool
	}{
		{"", []byte(doc), true},
		{"gzip", gz.Bytes(), true},
		{"deflate", zl.Bytes(), true},
		{"deflate", raw.Bytes(), true},
		{"br", []byte(doc), false},
	} {
		t.Run(tc.enc+strconv.Itoa(len(tc.body)), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.enc != "" {
					w.Header().Set("Content-Encoding", tc.enc)
				}
				_, _ = w.Write(tc.body)
			}))
			defer srv.Close()
			c, err := New(writeSessionFile(t, testSession()), WithBaseURL(srv.URL))
			if err != nil {
				t.Fatal(err)
			}
			v, err := c.GetStore(context.Background(), "example-market")
			if !tc.ok {
				if err == nil {
					t.Fatalf("unknown encoding accepted: %v", v)
				}
				return
			}
			if err != nil || get(v, "ok") != true {
				t.Fatalf("decoded %v, %v", v, err)
			}
		})
	}
}
