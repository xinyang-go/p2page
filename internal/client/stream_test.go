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
		length             int64
	}{
		{"small form", "application/x-www-form-urlencoded; charset=UTF-8", "a=b", 3},
		{"empty form", "application/x-www-form-urlencoded", "", 0},
		{"large form", "application/x-www-form-urlencoded", "a=" + strings.Repeat("x", formBufferLimit), 0},
		{"file", "application/octet-stream", "binary", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := upstreamBody(io.NopCloser(strings.NewReader(tc.payload)), tc.typ)
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

func TestReplyRewritesLocationAndCookiePath(t *testing.T) {
	base, _ := wire.ParseTarget("https://secure.lan/apps/demo")
	dest, _ := destination(base, "/path")
	h := http.Header{"Location": {"https://secure.lan/apps/demo/login?next=a"}, "Set-Cookie": {"sid=1; Path=/apps/demo; HttpOnly"}}
	v := replyHeaders(303, h, base, dest)
	if v.Headers.Get("Location") != "/login?next=a" || v.Headers.Get("Set-Cookie") != "" {
		t.Fatalf("reply: %+v", v)
	}
}
