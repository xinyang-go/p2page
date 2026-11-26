package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"p2page/internal/client"
)

func TestBrowser(t *testing.T) {
	if os.Getenv("P2PAGE_BROWSER_TEST") != "1" {
		t.Skip("set P2PAGE_BROWSER_TEST=1 to run the Chromium integration test")
	}
	for _, bin := range []string{"chromium", "node"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatal(err)
		}
	}
	s := testServer(t)
	var uid int64
	if err := s.db.QueryRow("SELECT id FROM users WHERE admin=1").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	u := account{ID: uid, Admin: true}
	secret, err := s.mutate(form(url.Values{"action": {"client-add"}, "name": {"fixture"}}), u)
	if err != nil {
		t.Fatal(err)
	}
	_, token, _ := strings.Cut(secret, ": ")
	block := bytes.Repeat([]byte("0123456789abcdef"), 512<<10)
	var clip atomic.Value
	var deepLinkReached atomic.Bool
	var loginPosts atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/style-burst/") {
			// Keep this burst in flight long enough to exercise the peer's
			// channel limit, instead of testing a series of finished requests.
			select {
			case <-time.After(350 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "text/css")
			_, _ = io.WriteString(w, "#fixture { outline-width: 3px; }")
			return
		}
		switch r.URL.Path {
		case "/":
			w.Header().Set("Location", "web")
			w.WriteHeader(http.StatusFound)
		case "/web":
			w.Header().Set("Location", (&url.URL{Scheme: "https", Host: r.Host, Path: "/web/"}).String())
			w.WriteHeader(http.StatusMovedPermanently)
		case "/web/", "/web/jellyfin", "/web/opencode/session":
			if r.URL.Path == "/web/opencode/session" && r.URL.Query().Get("client-only") == "spa" {
				deepLinkReached.Store(true)
			}
			http.SetCookie(w, &http.Cookie{Name: "appSession", Value: "persisted", Path: "/", HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "strictEntry", Value: "1", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Security-Policy", "default-src 'self'")
			_, _ = io.WriteString(w, `<!doctype html><html><title>Fixture</title><link rel="icon" href="data:image/svg+xml,%3Csvg%20xmlns='http://www.w3.org/2000/svg'/%3E"><h1 id="fixture">Application is live</h1>
				<form method="post" action="/cgi-bin/luci/"><input name="testuser" value="fixture"><button>Sign in</button></form>
				<a id="save" href="/download-redirect" download="fixture.bin">Download</a>
				<script src="/assets/static.js"></script></html>`)
		case "/assets/static.js":
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = io.WriteString(w, `document.body.dataset.staticAsset = "loaded";`)
		case "/cgi-bin/luci/":
			if r.ContentLength <= 0 {
				http.Error(w, "login requires Content-Length", http.StatusLengthRequired)
				return
			}
			if r.Method != "POST" || r.ParseForm() != nil || r.PostForm.Get("testuser") != "fixture" {
				http.Error(w, "invalid login form", http.StatusBadRequest)
				return
			}
			if r.Header.Get("Origin") != "https://"+r.Host || !strings.HasPrefix(r.Referer(), "https://"+r.Host+"/web/") {
				http.Error(w, "form origin or referrer missing", http.StatusBadRequest)
				return
			}
			loginPosts.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "routerSession", Value: "authenticated", Path: "/cgi-bin/luci/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			w.Header().Set("Location", "admin/?tab=home")
			w.WriteHeader(http.StatusSeeOther)
		case "/cgi-bin/luci/admin/":
			c, err := r.Cookie("routerSession")
			if r.Method != "GET" || err != nil || c.Value != "authenticated" || r.URL.RawQuery != "tab=home" {
				http.Error(w, "login cookie missing after redirect", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<!doctype html><html><h1 id="login-ok">Signed in</h1></html>`)
		case "/cookie", "/assets/api":
			c, err := r.Cookie("appSession")
			if err != nil {
				http.Error(w, "missing cookie", 400)
				return
			}
			_, _ = io.WriteString(w, c.Value)
		case "/set-visible":
			http.SetCookie(w, &http.Cookie{Name: "visible", Value: "from-response", Path: "/"})
			w.WriteHeader(204)
		case "/omit":
			if r.Header.Get("Cookie") != "" {
				http.Error(w, "omit sent cookies", 400)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "ignored", Value: "from-omit", Path: "/"})
			w.WriteHeader(204)
		case "/script-cookie":
			c, err := r.Cookie("saved")
			if err != nil {
				http.Error(w, "script cookie missing", 400)
				return
			}
			_, _ = io.WriteString(w, c.Value)
		case "/upload":
			n, err := io.Copy(io.Discard, r.Body)
			if err != nil {
				http.Error(w, "upload failed", 400)
				return
			}
			_, _ = fmt.Fprint(w, n)
		case "/request-metadata":
			_, _ = fmt.Fprintf(w, "%s|%s|%d", r.Header.Get("Origin"), r.Referer(), r.ContentLength)
		case "/frame-denied":
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<h1>Embeds denied</h1>`)
		case "/strict-start":
			http.Redirect(w, r, "/strict-end", http.StatusFound)
		case "/strict-end":
			w.Header().Set("Content-Type", "text/html")
			status := "withheld"
			if _, err := r.Cookie("strictEntry"); err == nil {
				status = "sent"
			}
			_, _ = fmt.Fprintf(w, `<h1 id="strict-check">%s</h1>`, status)
		case "/download":
			http.ServeContent(w, r, "test.bin", time.Time{}, bytes.NewReader(block))
		case "/download-redirect":
			http.Redirect(w, r, "/download", http.StatusFound)
		case "/video":
			if r.Method == "POST" {
				b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
				if err != nil || len(b) == 0 {
					http.Error(w, "invalid clip", 400)
					return
				}
				clip.Store(b)
				w.WriteHeader(204)
				return
			}
			b, _ := clip.Load().([]byte)
			w.Header().Set("Content-Type", "video/webm")
			http.ServeContent(w, r, "clip.webm", time.Time{}, bytes.NewReader(b))
		case "/redirect":
			http.Redirect(w, r, "/cookie", 303)
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-time.After(600 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, "data: second\n\n")
			w.(http.Flusher).Flush()
		case "/ws":
			if c, err := r.Cookie("appSession"); err != nil || c.Value != "persisted" {
				http.Error(w, "WebSocket cookie missing", 400)
				return
			}
			up := websocket.Upgrader{Subprotocols: []string{"demo"}}
			ws, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer ws.Close()
			for {
				t, b, err := ws.ReadMessage()
				if err != nil || ws.WriteMessage(t, b) != nil {
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	if _, err := s.mutate(form(url.Values{"action": {"app-add"}, "name": {"demo"}, "target": {upstream.URL}, "mode": {"stun"}, "client": {"1"}}), u); err != nil {
		t.Fatal(err)
	}
	var shellRequests, leakedAppRequests, leakedCookies atomic.Int32
	edge := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "outside.test" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<a href="https://demo-admin.example.test/strict-start">Open app</a>`)
			return
		}
		if r.URL.Path == "/__p2page/shell" {
			shellRequests.Add(1)
		}
		for _, c := range r.Cookies() {
			switch c.Name {
			case "appSession", "routerSession", "saved", "visible", "ignored", "strictEntry":
				leakedCookies.Add(1)
			}
		}
		if r.Header.Get("X-P2Page-Cookie") != "" || strings.Contains(r.URL.RawQuery, "client-only") ||
			strings.Contains(r.Referer(), "client-only") ||
			(r.URL.Path == "/__p2page/socket.js" && r.Referer() != "") ||
			(r.Host == "demo-admin.example.test" && r.URL.Path != "/" && r.URL.Path != "/favicon.ico" && !strings.HasPrefix(r.URL.Path, "/__p2page/")) {
			leakedAppRequests.Add(1)
		}
		if r.URL.Path == "/__p2page/agent" {
			// The Go fixture agent dials the test listener by IP, while Chromium
			// maps the public names to this listener through its resolver.
			r.Host = "example.test"
		}
		s.ServeHTTP(w, r)
	}))
	defer edge.Close()
	tmp := t.TempDir()
	cert := filepath.Join(tmp, "ca.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: edge.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", cert)
	cfg, _ := json.Marshal(client.Config{Server: edge.URL, Token: token, Upstreams: []client.Upstream{{URL: upstream.URL, CAFile: cert}}})
	cfgPath := filepath.Join(tmp, "client.json")
	if err := os.WriteFile(cfgPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	agentDone := make(chan error, 1)
	go func() { agentDone <- client.Run(ctx, cfgPath) }()
	defer func() { cancel(); <-agentDone }()
	var active bool
	for i := 0; i < 100; i++ {
		s.mu.Lock()
		active = s.agents[1] != nil
		s.mu.Unlock()
		if active {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !active {
		t.Fatal("agent did not attach")
	}
	p, _ := url.Parse(edge.URL)
	spki := sha256.Sum256(edge.Certificate().RawSubjectPublicKeyInfo)
	cmd := exec.CommandContext(ctx, "node", "testdata/browser.mjs", "https://demo-admin.example.test/#first-native", p.Port(),
		base64.StdEncoding.EncodeToString(spki[:]), tmp, strconv.Itoa(len(block)))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Chromium: %v\n%s", err, out)
	}
	if shellRequests.Load() == 0 || leakedAppRequests.Load() != 0 || leakedCookies.Load() != 0 || !deepLinkReached.Load() {
		t.Fatalf("unexpected network activity: shell=%d app requests=%d app cookies=%d deep link reached=%v",
			shellRequests.Load(), leakedAppRequests.Load(), leakedCookies.Load(), deepLinkReached.Load())
	}
	if n := loginPosts.Load(); n != 3 {
		t.Fatalf("expected exactly one upstream POST per form submission, got %d", n)
	}
	t.Logf("%s", out)
}
