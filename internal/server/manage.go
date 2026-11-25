package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
	"p2page/internal/wire"
)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS settings (name TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE, display TEXT NOT NULL,
	password TEXT NOT NULL, admin INTEGER NOT NULL DEFAULT 0, disabled INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX IF NOT EXISTS one_admin ON users(admin) WHERE admin=1;
CREATE TABLE IF NOT EXISTS invites (id INTEGER PRIMARY KEY AUTOINCREMENT, digest TEXT NOT NULL UNIQUE,
	expires INTEGER NOT NULL, used_by INTEGER REFERENCES users(id), revoked INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS sessions (digest TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS sessions_user_id ON sessions(user_id);
CREATE TABLE IF NOT EXISTS handoffs (nonce TEXT PRIMARY KEY, expires INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS clients (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE, UNIQUE(user_id,name), UNIQUE(user_id,id));
CREATE TABLE IF NOT EXISTS apps (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL, client_id INTEGER NOT NULL,
	name TEXT NOT NULL, display TEXT NOT NULL, target TEXT NOT NULL, public INTEGER NOT NULL, mode TEXT NOT NULL, ice TEXT NOT NULL,
	FOREIGN KEY(user_id,client_id) REFERENCES clients(user_id,id) ON DELETE CASCADE, UNIQUE(user_id,name));
`

func validPassword(p string) bool { return len(p) >= 12 && len(p) <= 72 && utf8.ValidString(p) }
func (s *Server) init() error {
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// Ensure connection-local pragmas also apply if the driver replaces the connection.
	if _, err := s.db.Exec("INSERT OR IGNORE INTO settings VALUES ('signing_key',?)", random(32)); err != nil {
		return err
	}
	var k string
	if err := s.db.QueryRow("SELECT value FROM settings WHERE name='signing_key'").Scan(&k); err != nil {
		return err
	}
	var err error
	s.key, err = b64.DecodeString(k)
	if err != nil || len(s.key) != 32 {
		return errors.New("invalid database signing key")
	}
	var existing string
	err = s.db.QueryRow("SELECT name FROM users WHERE admin=1").Scan(&existing)
	if err == nil {
		if existing != s.cfg.AdminUsername {
			return errors.New("admin_username does not match existing administrator")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	pw := os.Getenv("P2PAGE_ADMIN_PASSWORD")
	if !validPassword(pw) {
		return errors.New("first run requires P2PAGE_ADMIN_PASSWORD (12–72 bytes)")
	}
	p, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO users(name,display,password,admin) VALUES(?,?,?,1)", s.cfg.AdminUsername, "Administrator", p)
	return err
}

func (s *Server) lookup(name, owner string) (app, error) {
	var a app
	var ice string
	err := s.db.QueryRow(`SELECT a.id,a.client_id,a.name,a.display,u.name,a.target,a.public,a.mode,a.ice
	FROM apps a JOIN users u ON a.user_id=u.id WHERE a.name=? AND u.name=? AND NOT u.disabled`, name, owner).
		Scan(&a.ID, &a.ClientID, &a.Name, &a.Display, &a.Owner, &a.Target, &a.Public, &a.Mode, &ice)
	if err == nil {
		err = json.Unmarshal([]byte(ice), &a.ICE)
	}
	return a, err
}

func (s *Server) userBySession(sid string) (account, bool) {
	var a account
	if len(sid) != 64 {
		return a, false
	}
	err := s.db.QueryRow(`SELECT u.id,u.name,u.display,u.admin FROM sessions s JOIN users u ON s.user_id=u.id
	WHERE s.digest=? AND s.expires>? AND NOT u.disabled`, sid, time.Now().Unix()).Scan(&a.ID, &a.Name, &a.Display, &a.Admin)
	return a, err == nil
}

func authNext(p string) string {
	if len(p) <= 8192 && strings.HasPrefix(p, "/authorize?") && !strings.ContainsAny(p, "\r\n\\") {
		return p
	}
	return "/dashboard"
}

func (s *Server) manage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	if r.URL.Path == "/__p2page/agent" && r.Method == "GET" {
		s.attach(w, r)
		return
	}
	if r.URL.Path == "/__p2page/style.css" {
		asset(w, r, "style.css")
		return
	}
	if r.URL.Path == "/__p2page/login.js" {
		asset(w, r, "login.js")
		return
	}
	if r.Method != "GET" && r.Method != "POST" {
		bad(w, 405, "Method not allowed")
		return
	}
	if r.Method == "POST" {
		if !sameOrigin(r) {
			bad(w, 403, "Forbidden")
			return
		}
		r.Body = body(w, r, 32<<10)
		if r.ParseForm() != nil {
			bad(w, 400, "Invalid form")
			return
		}
	}
	sid := hash(cookie(r, sessionCookie))
	u, logged := s.userBySession(sid)
	switch r.URL.Path {
	case "/", "/login", "/register":
		next := authNext(r.FormValue("next"))
		if logged {
			http.Redirect(w, r, next, 303)
			return
		}
		mode := "login"
		if r.URL.Path == "/register" {
			mode = "register"
		}
		info := map[string]string{"Mode": mode, "Next": next}
		if r.Method == "GET" {
			page(w, "auth.html", info)
			return
		}
		name := r.PostForm.Get("name")
		if !s.allow("auth:"+s.remote(r), 120, time.Minute) || (nameRE.MatchString(name) && !s.allow("auth-user:"+name, 12, 5*time.Minute)) {
			bad(w, 429, "Too many attempts")
			return
		}
		select {
		case s.authWork <- struct{}{}:
			defer func() { <-s.authWork }()
		default:
			bad(w, 429, "Authentication is busy; retry shortly")
			return
		}
		id, err := s.authenticate(r, mode)
		if err != nil {
			info["Error"] = "Invalid credentials, username, or invitation. Passwords must be 12–72 bytes."
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(400)
			page(w, "auth.html", info)
			return
		}
		raw := random(32)
		if _, err = s.db.Exec("INSERT INTO sessions VALUES(?,?,?)", hash(raw), id, time.Now().Add(7*24*time.Hour).Unix()); err != nil {
			bad(w, 503, "Cannot create session")
			return
		}
		secureCookie(w, sessionCookie, raw, 7*86400)
		http.Redirect(w, r, next, 303)
	case "/dashboard":
		if !logged {
			http.Redirect(w, r, "/", 303)
			return
		}
		if r.Method == "POST" {
			if !s.allow("configure:"+u.Name, 120, time.Minute) {
				bad(w, 429, "Too many configuration requests")
				return
			}
			secret, err := s.mutate(r, u)
			if err != nil {
				s.dashboard(w, u, "", err.Error())
				return
			}
			if secret != "" {
				s.dashboard(w, u, secret, "")
				return
			}
			http.Redirect(w, r, "/dashboard", 303)
			return
		}
		s.dashboard(w, u, "", "")
	case "/logout":
		if r.Method != "POST" {
			bad(w, 405, "Method not allowed")
			return
		}
		if _, err := s.db.Exec("DELETE FROM sessions WHERE digest=?", sid); err != nil {
			bad(w, 503, "Logout failed")
			return
		}
		secureCookie(w, sessionCookie, "", -1)
		http.Redirect(w, r, "/", 303)
	case "/authorize":
		if r.Method != "GET" {
			bad(w, 405, "Method not allowed")
			return
		}
		if !logged {
			http.Redirect(w, r, "/login?"+url.Values{"next": {r.URL.RequestURI()}}.Encode(), 303)
			return
		}
		name, owner, ok := strings.Cut(r.URL.Query().Get("app"), "-")
		if !ok || !nameRE.MatchString(name) || owner != u.Name {
			bad(w, 403, "Sign in as the application owner")
			return
		}
		a, err := s.lookup(name, owner)
		if err != nil {
			bad(w, 404, "Application not found")
			return
		}
		dest := "https://" + name + "-" + owner + "." + s.cfg.Domain
		t := s.sign("handoff", grant{App: a.ID, SID: sid, Next: localPath(r.URL.Query().Get("next")), Nonce: random(24), Exp: time.Now().Add(time.Minute).Unix()})
		nonce := random(18)
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'nonce-"+nonce+"'; frame-ancestors 'none'; base-uri 'none'; form-action "+dest)
		page(w, "handoff.html", map[string]string{"Action": dest + wire.Prefix + "authorize", "Ticket": t, "Nonce": nonce})
	default:
		bad(w, 404, "Not found")
	}
}

var dummyPassword = []byte("$2a$10$AIy6uUqHy5Iv8mQHdWle6uQQwX/drl6V6inwRMx8Mmr2ncEk1r0GO")
var invalidCredentials = errors.New("invalid credentials")

func (s *Server) authenticate(r *http.Request, mode string) (int64, error) {
	name, pw := r.PostForm.Get("name"), r.PostForm.Get("password")
	if !nameRE.MatchString(name) || !validPassword(pw) {
		return 0, invalidCredentials
	}
	var id int64
	if mode == "login" {
		p := dummyPassword
		var disabled bool
		err := s.db.QueryRow("SELECT id,password,disabled FROM users WHERE name=?", name).Scan(&id, &p, &disabled)
		if bcrypt.CompareHashAndPassword(p, []byte(pw)) != nil || err != nil || disabled {
			return 0, invalidCredentials
		}
		return id, nil
	}
	display, invite := r.PostForm.Get("display"), r.PostForm.Get("invite")
	if display == "" {
		display = name
	}
	if len(display) > 256 || len(invite) != 43 {
		return 0, invalidCredentials
	}
	// Preflight avoids an expensive hash for a random invite. The transaction still consumes it atomically.
	var available int
	q := "SELECT 1 FROM invites WHERE digest=? AND expires>? AND NOT revoked AND used_by IS NULL"
	if s.db.QueryRow(q, hash(invite), time.Now().Unix()).Scan(&available) != nil {
		return 0, invalidCredentials
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec("INSERT INTO users(name,display,password) VALUES(?,?,?)", name, display, h)
	if err != nil {
		return 0, err
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, err
	}
	res, err = tx.Exec("UPDATE invites SET used_by=? WHERE digest=? AND expires>? AND NOT revoked AND used_by IS NULL", id, hash(invite), time.Now().Unix())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return 0, invalidCredentials
	}
	return id, tx.Commit()
}

type clientRow struct {
	ID     int64
	Name   string
	Online bool
}
type appRow struct {
	app
	URL, ICEText string
}
type memberRow struct {
	ID              int64
	Name, Display   string
	Disabled, Admin bool
}
type inviteRow struct {
	ID              int64
	Expires, Status string
}
type dashboard struct {
	User                  account
	Domain, Secret, Error string
	Clients               []clientRow
	Apps                  []appRow
	Members               []memberRow
	Invites               []inviteRow
}

func rowsOf[T any](db *sql.DB, q string, scan func(*sql.Rows, *T) error, args ...any) ([]T, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var v T
		if err := scan(rows, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Server) dashboard(w http.ResponseWriter, u account, secret, msg string) {
	d := dashboard{User: u, Domain: s.cfg.Domain, Secret: secret, Error: msg}
	err := s.loadDashboard(&d)
	if err != nil {
		log.Printf("dashboard query: %v", err)
		bad(w, 503, "Unavailable")
		return
	}
	if msg != "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(400)
	}
	page(w, "dashboard.html", &d)
}
func (s *Server) loadDashboard(d *dashboard) error {
	var err error
	d.Clients, err = rowsOf(s.db, "SELECT id,name FROM clients WHERE user_id=? ORDER BY id", func(r *sql.Rows, c *clientRow) error {
		if err := r.Scan(&c.ID, &c.Name); err != nil {
			return err
		}
		s.mu.Lock()
		c.Online = s.agents[c.ID] != nil
		s.mu.Unlock()
		return nil
	}, d.User.ID)
	if err != nil {
		return err
	}
	d.Apps, err = rowsOf(s.db, "SELECT id,client_id,name,display,target,public,mode,ice FROM apps WHERE user_id=? ORDER BY id", func(r *sql.Rows, a *appRow) error {
		if err := r.Scan(&a.ID, &a.ClientID, &a.Name, &a.Display, &a.Target, &a.Public, &a.Mode, &a.ICEText); err != nil {
			return err
		}
		a.URL = "https://" + a.Name + "-" + d.User.Name + "." + s.cfg.Domain
		var ice []wire.ICE
		if err := json.Unmarshal([]byte(a.ICEText), &ice); err != nil {
			return err
		}
		p, err := json.MarshalIndent(ice, "", "  ")
		a.ICEText = string(p)
		return err
	}, d.User.ID)
	if err != nil || !d.User.Admin {
		return err
	}
	d.Members, err = rowsOf(s.db, "SELECT id,name,display,disabled,admin FROM users ORDER BY id", func(r *sql.Rows, u *memberRow) error {
		return r.Scan(&u.ID, &u.Name, &u.Display, &u.Disabled, &u.Admin)
	})
	if err != nil {
		return err
	}
	d.Invites, err = rowsOf(s.db, "SELECT id,expires,used_by,revoked FROM invites ORDER BY id DESC LIMIT 200", func(r *sql.Rows, i *inviteRow) error {
		var used sql.NullInt64
		var expires int64
		var revoked bool
		if err := r.Scan(&i.ID, &expires, &used, &revoked); err != nil {
			return err
		}
		i.Expires = time.Unix(expires, 0).UTC().Format("2006-01-02 15:04 UTC")
		switch {
		case used.Valid:
			i.Status = "Used"
		case revoked:
			i.Status = "Revoked"
		case expires <= time.Now().Unix():
			i.Status = "Expired"
		default:
			i.Status = "Active"
		}
		return nil
	})
	return err
}

func validateICE(mode, raw string) ([]wire.ICE, error) {
	var ice []wire.ICE
	if mode != "stun" && mode != "turn" {
		return nil, errors.New("Choose STUN or TURN mode")
	}
	if strings.TrimSpace(raw) != "" {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if len(raw) > 10<<10 || dec.Decode(&ice) != nil || !json.Valid([]byte(raw)) {
			return nil, errors.New("Invalid ICE configuration JSON")
		}
	}
	if ice == nil {
		ice = []wire.ICE{}
	}
	total := 0
	turn := false
	for _, i := range ice {
		if len(i.URLs) == 0 || len(i.Username) > 256 || len(i.Credential) > 512 {
			return nil, errors.New("Invalid ICE server")
		}
		for _, u := range i.URLs {
			total++
			p, err := url.Parse(u)
			if total > 8 || len(u) > 256 || err != nil || p.Opaque == "" || strings.ContainsAny(p.Opaque, " /@\\") || p.Fragment != "" ||
				(p.RawQuery != "" && p.RawQuery != "transport=tcp" && p.RawQuery != "transport=udp") {
				return nil, errors.New("Use at most eight stun:, stuns:, turn: or turns: URLs")
			}
			switch p.Scheme {
			case "stun", "stuns":
				if p.RawQuery != "" {
					return nil, errors.New("STUN URLs cannot have query parameters")
				}
			case "turn", "turns":
				if mode == "stun" || i.Username == "" || i.Credential == "" {
					return nil, errors.New("TURN requires TURN mode and credentials")
				}
				turn = true
			default:
				return nil, errors.New("Invalid ICE scheme")
			}
		}
	}
	if mode == "turn" && !turn {
		return nil, errors.New("TURN mode requires at least one TURN server")
	}
	return ice, nil
}
func (s *Server) mutate(r *http.Request, u account) (string, error) {
	f := r.PostForm.Get
	id, _ := strconv.ParseInt(f("id"), 10, 64)
	exec := func(q string, args ...any) error {
		if _, err := s.db.Exec(q, args...); err != nil {
			log.Printf("configuration write: %v", err)
			return errors.New("Change rejected; check names and client selection")
		}
		return nil
	}
	switch f("action") {
	case "client-add":
		n := strings.TrimSpace(f("name"))
		if len(n) == 0 || len(n) > 128 {
			return "", errors.New("Client label is required (up to 128 bytes)")
		}
		token := random(32)
		if err := exec("INSERT INTO clients(user_id,name,token_hash) VALUES(?,?,?)", u.ID, n, hash(token)); err != nil {
			return "", err
		}
		return "Client token (save now): " + token, nil
	case "client-delete":
		if err := exec("DELETE FROM clients WHERE id=? AND user_id=?", id, u.ID); err != nil {
			return "", err
		}
		s.disconnectUnauthorized()
		return "", nil
	case "app-add", "app-edit":
		n, d, target, mode := f("name"), f("display"), strings.TrimSpace(f("target")), f("mode")
		if d == "" {
			d = n
		}
		if !nameRE.MatchString(n) || len(d) > 256 || len(target) > 2048 {
			return "", errors.New("App name must contain 1–30 lowercase letters or digits")
		}
		if _, err := wire.ParseTarget(target); err != nil {
			return "", err
		}
		cid, _ := strconv.ParseInt(f("client"), 10, 64)
		ice, err := validateICE(mode, f("ice"))
		if err != nil {
			return "", err
		}
		p, _ := json.Marshal(ice)
		args := []any{cid, n, d, target, f("public") == "on", mode, string(p), u.ID}
		if f("action") == "app-add" {
			return "", exec("INSERT INTO apps(client_id,name,display,target,public,mode,ice,user_id) VALUES(?,?,?,?,?,?,?,?)", args...)
		}
		return "", exec("UPDATE apps SET client_id=?,name=?,display=?,target=?,public=?,mode=?,ice=? WHERE user_id=? AND id=?", append(args, id)...)
	case "app-delete":
		return "", exec("DELETE FROM apps WHERE id=? AND user_id=?", id, u.ID)
	case "invite-add", "invite-revoke", "user-disable", "user-enable":
		if !u.Admin {
			return "", errors.New("Administrator access required")
		}
	default:
		return "", errors.New("Unknown action")
	}
	switch f("action") {
	case "invite-add":
		raw := random(32)
		if err := exec("INSERT INTO invites(digest,expires) VALUES(?,?)", hash(raw), time.Now().Add(7*24*time.Hour).Unix()); err != nil {
			return "", err
		}
		return "Invitation (save now; expires in 7 days): " + raw, nil
	case "invite-revoke":
		return "", exec("UPDATE invites SET revoked=1 WHERE id=? AND used_by IS NULL", id)
	case "user-disable", "user-enable":
		disabled := f("action") == "user-disable"
		tx, err := s.db.Begin()
		if err != nil {
			return "", errors.New("User update failed")
		}
		defer tx.Rollback()
		_, err = tx.Exec("UPDATE users SET disabled=? WHERE id=? AND NOT admin", disabled, id)
		if err == nil && disabled {
			_, err = tx.Exec("DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE id=? AND disabled)", id)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			return "", errors.New("User update failed")
		}
		if disabled {
			s.disconnectUnauthorized()
		}
		return "", nil
	}
	return "", fmt.Errorf("invalid action")
}

func (s *Server) disconnectUnauthorized() {
	s.mu.Lock()
	ids := make(map[int64]*agent, len(s.agents))
	for id, a := range s.agents {
		ids[id] = a
	}
	s.mu.Unlock()
	for id, a := range ids {
		var ok int
		if errors.Is(s.db.QueryRow("SELECT 1 FROM clients c JOIN users u ON u.id=c.user_id WHERE c.id=? AND NOT u.disabled", id).Scan(&ok), sql.ErrNoRows) {
			_ = a.conn.Close()
		}
	}
}
