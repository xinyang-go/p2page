package client

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestCookieJar(t *testing.T) {
	now := time.Now()
	j := &cookieJar{}
	j.set([]string{
		"sid=private; Path=/apps/demo; HttpOnly",
		"shared=one; Path=/; Domain=upstream.lan",
		"scoped=two; Path=/apps/demo/admin/",
		"defaulted=three",
		"other=ignored; Path=/apps/other",
		"old=expired; Max-Age=0",
		"__Host-p2page-grant=ignored; Path=/; Secure",
	}, "/nested/login", "/apps/demo", false, now)
	if got := j.header("/admin/panel"); got != "scoped=two; sid=private; shared=one" {
		t.Fatalf("unexpected cookie header: %q", got)
	}
	if got := j.header("/nested/page"); got != "defaulted=three; sid=private; shared=one" {
		t.Fatalf("unexpected default path: %q", got)
	}
	if got := j.header("/nestedElsewhere"); got != "sid=private; shared=one" {
		t.Fatalf("unexpected cookie across path boundary: %q", got)
	}
	j.set([]string{"sid=guessed; Path=/", "sid=; Max-Age=0; Path=/", "shared=; Max-Age=0; Path=/", "js=allowed; Path=/"}, "/", "", true, now)
	if got := j.header("/"); got != "sid=private; js=allowed" {
		t.Fatalf("script mutated HttpOnly cookie: %q", got)
	}
	state := j.state(nil)
	for _, c := range state.Cookies {
		if c.Name == "sid" || strings.Contains(c.Value, "private") {
			t.Fatal("HttpOnly leaked into script state")
		}
	}
	if duplicate := j.state(&state.Revision); duplicate != nil {
		t.Fatal("unchanged state was resent")
	}
	j.set([]string{"sid=; Max-Age=0; Path=/"}, "/", "", false, now)
	if got := j.header("/"); got != "js=allowed" {
		t.Fatalf("upstream logout did not remove cookie: %q", got)
	}
}

func TestCookieSessionScopeAndReconnect(t *testing.T) {
	capability := func(ch byte) string {
		return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{ch}, 32))
	}
	s := &sessionStore{}
	acquire := func(app int64, target, cp string, timeOffset time.Duration) (*cookieJar, func()) {
		t.Helper()
		j, release, err := s.acquire(app, target, cp, time.Now().Add(timeOffset))
		if err != nil {
			t.Fatal(err)
		}
		return j, release
	}
	first, release := acquire(1, "http://device.lan", capability(1), 0)
	first.set([]string{"sid=private; Path=/"}, "/", "", false, time.Now())
	release()
	again, release := acquire(1, "http://device.lan", capability(1), 0)
	if got := again.header("/"); got != "sid=private" {
		t.Fatal("browser reconnect lost cookie")
	}
	release()
	for _, scope := range []struct {
		app    int64
		target string
		cap    string
		offset time.Duration
	}{
		{2, "http://device.lan", capability(1), 0},
		{1, "http://other.lan", capability(1), 0},
		{1, "http://device.lan", capability(2), 0},
		{1, "http://device.lan", capability(1), sessionIdle + time.Second},
	} {
		other, done := acquire(scope.app, scope.target, scope.cap, scope.offset)
		if other.header("/") != "" {
			t.Fatal("cookie leaked across session, app, upstream or idle expiry")
		}
		done()
	}
}
