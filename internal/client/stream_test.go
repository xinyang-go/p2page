package client

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"p2page/internal/wire"
)

func TestDestination(t *testing.T) {
	u, err := wire.ParseTarget("https://secure.lan:8443/apps/demo/")
	if err != nil {
		t.Fatal(err)
	}
	got, err := destination(u, "/video/file%20name?seek=42")
	if err != nil || got.String() != "https://secure.lan:8443/apps/demo/video/file%20name?seek=42" {
		t.Fatalf("destination: %v %v", got, err)
	}
	for _, s := range []string{"https://evil.test/", "//evil.test/", "/../", "/%2e%2e/x", "/%5cevil", "/__p2page/shell"} {
		if _, err := destination(u, s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestHTMLInjection(t *testing.T) {
	for _, s := range []string{"<!doctype html>", "  <!DOCTYPE html>", "\xef\xbb\xbf<!doctype html>", ""} {
		b, err := io.ReadAll(injectHTML(strings.NewReader(s + "<html>hello</html>")))
		if err != nil || !strings.HasPrefix(string(b), s+`<script src="/__p2page/socket.js" referrerpolicy="no-referrer"></script>`) {
			t.Fatalf("injection: %q %v", b, err)
		}
	}
}

func TestUpstreamFormBody(t *testing.T) {
	for _, tc := range []struct {
		name, typ, payload string
		keepalive          bool
		length             int64
	}{
		{"small form", "application/x-www-form-urlencoded; charset=UTF-8", "a=b", false, 3},
		{"empty form", "application/x-www-form-urlencoded", "", false, 0},
		{"large form", "application/x-www-form-urlencoded", "a=" + strings.Repeat("x", formBufferLimit), false, 0},
		{"multipart form", "multipart/form-data; boundary=abc", "--abc--\r\n", false, 9},
		{"large multipart form", "multipart/form-data; boundary=abc", strings.Repeat("x", formBufferLimit+1), false, 0},
		{"beacon", "text/plain", "hello", true, 5},
		{"file", "application/octet-stream", "binary", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := upstreamBody(io.NopCloser(strings.NewReader(tc.payload)), tc.typ, tc.keepalive)
			if err != nil {
				t.Fatal(err)
			}
			r, err := http.NewRequest("POST", "http://upstream.test/", body)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Body.Close()
			got, err := io.ReadAll(r.Body)
			if err != nil || string(got) != tc.payload || r.ContentLength != tc.length {
				t.Fatalf("read %d bytes, content length %d: %v", len(got), r.ContentLength, err)
			}
		})
	}
}

func TestRequestOrigin(t *testing.T) {
	dest, _ := wire.ParseTarget("http://internal.test:8080")
	const public = "https://app.example.test"
	input := http.Header{
		"Origin": {public}, "Referer": {public + "/page?x=1"},
		"Host": {"spoof.test"}, "X-Real-Ip": {"127.0.0.1"}, "X-Forwarded-For": {"127.0.0.1"},
	}
	for _, tc := range []struct {
		public       bool
		origin, host string
	}{
		{false, dest.String(), ""},
		{true, public, "app.example.test"},
	} {
		origin, host := requestOrigin(&upstream{publicOriginHeaders: tc.public}, dest, public)
		headers := proxyHeaders(input, public, origin)
		if origin != tc.origin || host != tc.host || headers.Get("Origin") != tc.origin ||
			headers.Get("Referer") != tc.origin+"/page?x=1" || headers.Get("Host") != "" ||
			headers.Get("X-Real-IP") != "" || headers.Get("X-Forwarded-For") != "" {
			t.Fatalf("public=%t: origin=%q host=%q headers=%v", tc.public, origin, host, headers)
		}
	}
}

func TestReplyRewritesLocationAndCookiePath(t *testing.T) {
	base, _ := wire.ParseTarget("https://secure.lan/apps/demo")
	dest, _ := destination(base, "/path")
	h := http.Header{"Location": {"https://secure.lan/apps/demo/login?next=a"}, "Set-Cookie": {"sid=1; Path=/apps/demo; HttpOnly"}}
	v := replyHeaders(303, h, base, dest)
	if v.Headers.Get("Location") != "/login?next=a" || v.Headers.Get("Set-Cookie") != "" {
		t.Fatalf("reply: %+v", v)
	}
}
