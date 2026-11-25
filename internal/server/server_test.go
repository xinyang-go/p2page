package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"p2page/internal/wire"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("P2PAGE_ADMIN_PASSWORD", "initial passphrase")
	s, err := open(Config{Domain: "example.test", Database: filepath.Join(t.TempDir(), "test.db"), AdminUsername: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Close() })
	return s
}

func form(v url.Values) *http.Request {
	r := httptest.NewRequest("POST", "https://example.test/register", strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = r.ParseForm()
	return r
}

func TestInviteOnceConcurrent(t *testing.T) {
	s := testServer(t)
	var uid int64
	if err := s.db.QueryRow("SELECT id FROM users WHERE name='admin'").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	secret, err := s.mutate(form(url.Values{"action": {"invite-add"}}), account{ID: uid, Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	_, invite, _ := strings.Cut(secret, ": ")
	var wg sync.WaitGroup
	result := make(chan error, 2)
	for _, n := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.authenticate(form(url.Values{"name": {n}, "invite": {invite}, "password": {"new strong password"}}), "register")
			result <- err
		}()
	}
	wg.Wait()
	close(result)
	var success int
	for err := range result {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("registrations succeeded: %d", success)
	}
	var users int
	if err := s.db.QueryRow("SELECT count(*) FROM users").Scan(&users); err != nil || users != 2 {
		t.Fatalf("users=%d err=%v", users, err)
	}
}

func TestDashboardAndPrivateGrant(t *testing.T) {
	s := testServer(t)
	r := form(url.Values{"name": {"admin"}, "password": {"initial passphrase"}})
	r.URL.Path = "/login"
	r.Header.Set("Origin", "https://evil.test")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("CSRF status %d", w.Code)
	}
	r.Header.Set("Origin", "https://example.test")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 303 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	sess := w.Result().Cookies()[0]
	if sess.Domain != "" || !sess.Secure || !sess.HttpOnly {
		t.Fatalf("unsafe cookie: %v", sess)
	}
	u, ok := s.userBySession(hash(sess.Value))
	if !ok {
		t.Fatal("session was not stored")
	}
	put := func(action string, v url.Values) {
		t.Helper()
		v.Set("action", action)
		if _, err := s.mutate(form(v), u); err != nil {
			t.Fatal(err)
		}
	}
	put("client-add", url.Values{"name": {"home"}})
	put("app-add", url.Values{"name": {"demo"}, "display": {"<private>"}, "target": {"http://router.lan"}, "mode": {"stun"}, "client": {"1"}})
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "https://example.test/dashboard", nil)
	r.AddCookie(sess)
	s.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "&lt;private&gt;") || !strings.Contains(w.Body.String(), "New application") {
		t.Fatalf("dashboard incomplete:\n%s", w.Body)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "https://demo-admin.example.test/a?b=c", nil))
	if w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "https://example.test/authorize?") {
		t.Fatalf("private status: %d", w.Code)
	}
	a, err := s.lookup("demo", "admin")
	if err != nil {
		t.Fatal(err)
	}
	ticket := s.sign("handoff", grant{App: a.ID, SID: hash(sess.Value), Next: "/a?b=c", Nonce: "test", Exp: time.Now().Add(time.Minute).Unix()})
	handoff := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "https://demo-admin.example.test/__p2page/authorize", strings.NewReader(url.Values{"ticket": {ticket}}.Encode()))
		r.Header.Set("Origin", "https://example.test")
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.ServeHTTP(w, r)
		return w
	}
	w = handoff()
	if w.Code != 303 || w.Header().Get("Location") != "/a?b=c" {
		t.Fatalf("handoff: %d %s", w.Code, w.Body)
	}
	grant := w.Result().Cookies()[0]
	if status := handoff().Code; status != 403 {
		t.Fatalf("handoff replay: %d", status)
	}
	r = httptest.NewRequest("GET", "https://demo-admin.example.test/a?b=c", nil)
	r.AddCookie(grant)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<iframe") {
		t.Fatalf("authorized shell: %d %s", w.Code, w.Body)
	}
	put("client-delete", url.Values{"id": {"1"}})
	if _, err := s.lookup("demo", "admin"); err != sql.ErrNoRows {
		t.Fatalf("app was not deleted: %v", err)
	}
}

func TestValidateICE(t *testing.T) {
	turn, _ := json.Marshal([]wire.ICE{{URLs: []string{"turn:turn.example.test:3478?transport=udp"}, Username: "user", Credential: "secret"}})
	if _, err := validateICE("stun", string(turn)); err == nil {
		t.Fatal("TURN accepted in STUN-only mode")
	}
	if _, err := validateICE("turn", string(turn)); err != nil {
		t.Fatal(err)
	}
	if _, err := validateICE("turn", `[{"urls":["stun:stun.example.test:3478"]}]`); err == nil {
		t.Fatal("TURN mode without TURN server accepted")
	}
}
