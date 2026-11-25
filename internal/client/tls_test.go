package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"p2page/internal/wire"
)

func upstreamConfigFile(t *testing.T, u Upstream) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client.json")
	b, err := json.Marshal(Config{Server: "https://example.test", Token: strings.Repeat("x", 43), Upstreams: []Upstream{u}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUpstreamTLSVerificationOptIn(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			upgrader := websocket.Upgrader{}
			if conn, err := upgrader.Upgrade(w, r, nil); err == nil {
				_ = conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer s.Close()
	target, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	wsURL := (&url.URL{Scheme: "wss", Host: target.Host, Path: "/ws"}).String()
	for _, tc := range []struct {
		name string
		skip bool
	}{{"default verifies", false}, {"explicit skip", true}} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := load(upstreamConfigFile(t, Upstream{URL: s.URL, InsecureSkipVerify: tc.skip}))
			if err != nil {
				t.Fatal(err)
			}
			up := c.origins[wire.Origin(target)]
			defer up.tr.CloseIdleConnections()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := up.tr.RoundTrip(req)
			if res != nil {
				_ = res.Body.Close()
			}
			if (err == nil) != tc.skip {
				t.Fatalf("unexpected HTTPS verification result: %v", err)
			}
			conn, res, err := up.ws.DialContext(ctx, wsURL, nil)
			if conn != nil {
				_ = conn.Close()
			}
			if res != nil {
				_ = res.Body.Close()
			}
			if (err == nil) != tc.skip {
				t.Fatalf("unexpected WSS verification result: %v", err)
			}
		})
	}
}

func TestInvalidInsecureUpstreamConfig(t *testing.T) {
	for _, u := range []Upstream{
		{URL: "http://nas.test", InsecureSkipVerify: true},
		{URL: "https://nas.test", CAFile: "ca.pem", InsecureSkipVerify: true},
	} {
		if _, err := load(upstreamConfigFile(t, u)); err == nil || !strings.Contains(err.Error(), "insecure_skip_verify") {
			t.Fatalf("expected configuration error for %s; got %v", u.URL, err)
		}
	}
}
