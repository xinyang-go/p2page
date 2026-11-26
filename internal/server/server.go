package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
	"p2page/internal/wire"
)

//go:embed web/*
var web embed.FS

var pages = template.Must(template.New("").Funcs(template.FuncMap{"indexApp": func() appRow { return appRow{} }}).ParseFS(web, "web/*.html"))
var nameRE = regexp.MustCompile(`^[a-z0-9]{1,30}$`)
var hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
var b64 = base64.RawURLEncoding

func validDomain(domain string) bool {
	if !hostRE.MatchString(domain) || !strings.Contains(domain, ".") || len(domain) > 180 {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
	}
	return true
}

const (
	sessionCookie = "__Host-p2page-session"
	grantCookie   = "__Host-p2page-grant"
	peerTTL       = 2 * time.Hour
)

type Config struct {
	Listen        string `json:"listen"`
	Domain        string `json:"domain"`
	Database      string `json:"database"`
	AdminUsername string `json:"admin_username"`
	TrustedProxy  string `json:"trusted_proxy_cidr,omitempty"`
}

type Server struct {
	cfg      Config
	db       *sql.DB
	key      []byte
	proxy    *net.IPNet
	mu       sync.Mutex
	agents   map[int64]*agent
	limits   map[string]limit
	authWork chan struct{}
}

type limit struct {
	n     int
	until time.Time
}
type account struct {
	ID            int64
	Name, Display string
	Admin         bool
}
type app struct {
	ID, ClientID                       int64
	Name, Display, Owner, Target, Mode string
	Public                             bool
	ICE                                []wire.ICE
}

type browserSettings struct {
	ICE  []wire.ICE `json:"ice"`
	Mode string     `json:"mode"`
}

func (a app) browserSettings() browserSettings {
	ice := a.ICE
	if ice == nil {
		ice = []wire.ICE{}
	}
	return browserSettings{ICE: ice, Mode: a.Mode}
}

func Run(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return err
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8080"
	}
	if cfg.Database == "" || !validDomain(cfg.Domain) ||
		!nameRE.MatchString(cfg.AdminUsername) {
		return errors.New("invalid domain, database or admin_username")
	}
	s, err := open(cfg)
	if err != nil {
		return err
	}
	defer s.db.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.maintain(ctx)
	h := &http.Server{Addr: cfg.Listen, Handler: s, ReadHeaderTimeout: 8 * time.Second, ReadTimeout: 50 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	shutdown := make(chan struct{})
	go func() {
		defer close(shutdown)
		<-ctx.Done()
		s.mu.Lock()
		for _, a := range s.agents {
			_ = a.conn.Close()
		}
		s.mu.Unlock()
		stop, done := context.WithTimeout(context.Background(), 8*time.Second)
		defer done()
		_ = h.Shutdown(stop)
	}()
	log.Printf("serving %s on %s (behind HTTPS proxy)", cfg.Domain, cfg.Listen)
	err = h.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdown
		return nil
	}
	return err
}

func open(cfg Config) (*Server, error) {
	path, err := filepath.Abs(cfg.Database)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Server{cfg: cfg, db: db, agents: map[int64]*agent{}, limits: map[string]limit{}, authWork: make(chan struct{}, 8)}
	if cfg.TrustedProxy != "" {
		_, s.proxy, err = net.ParseCIDR(cfg.TrustedProxy)
		if err != nil {
			db.Close()
			return nil, errors.New("invalid trusted_proxy_cidr")
		}
	}
	if err = s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b64.EncodeToString(b)
}
func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func (s *Server) mac(purpose, data string) []byte {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte(purpose + "\x00" + data))
	return h.Sum(nil)
}
func (s *Server) sign(purpose string, v any) string {
	p, _ := json.Marshal(v)
	m := b64.EncodeToString(p)
	return m + "." + b64.EncodeToString(s.mac(purpose, m))
}
func (s *Server) verify(purpose, token string, out any) bool {
	p, sig, ok := strings.Cut(token, ".")
	if !ok || len(token) > 8192 {
		return false
	}
	b, err := b64.DecodeString(sig)
	if err != nil || !hmac.Equal(b, s.mac(purpose, p)) {
		return false
	}
	b, err = b64.DecodeString(p)
	return err == nil && json.Unmarshal(b, out) == nil
}

func secureCookie(w http.ResponseWriter, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: age})
}
func cookie(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}
func sameOrigin(r *http.Request) bool                 { return r.Header.Get("Origin") == "https://"+r.Host }
func bad(w http.ResponseWriter, code int, msg string) { http.Error(w, msg, code) }
func (s *Server) allow(key string, max int, interval time.Duration) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.limits[key]
	if now.After(l.until) {
		if len(s.limits) >= 30000 {
			for k, v := range s.limits {
				if now.After(v.until) {
					delete(s.limits, k)
				}
			}
			if len(s.limits) >= 30000 {
				return false
			}
		}
		l = limit{until: now.Add(interval)}
	}
	if l.n >= max {
		return false
	}
	l.n++
	s.limits[key] = l
	return true
}
func (s *Server) remote(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	if s.proxy != nil && s.proxy.Contains(net.ParseIP(ip)) {
		if original := net.ParseIP(r.Header.Get("X-Real-IP")); original != nil {
			return original.String()
		}
	}
	return ip
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Origin-Agent-Cluster", "?1")
	w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	w.Header().Set("Cache-Control", "no-store")
	if r.Host == s.cfg.Domain {
		s.manage(w, r)
		return
	}
	sub, ok := strings.CutSuffix(r.Host, "."+s.cfg.Domain)
	name, user, split := strings.Cut(sub, "-")
	if !ok || !split || !nameRE.MatchString(name) || !nameRE.MatchString(user) {
		bad(w, 404, "Not found")
		return
	}
	a, err := s.lookup(name, user)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("app lookup: %v", err)
			bad(w, 503, "Unavailable")
			return
		}
		bad(w, 404, "Application not found")
		return
	}
	if r.URL.Path == wire.Prefix+"authorize" {
		s.acceptGrant(w, r, a)
		return
	}
	if r.URL.Path == wire.Prefix+"reauthorize" && r.Method == "GET" {
		// The destination stays in the tab's session storage.
		dest := "https://" + s.cfg.Domain + "/authorize?" + url.Values{"app": {sub}, "next": {"/"}}.Encode()
		w.Header().Set("Referrer-Policy", "origin")
		http.Redirect(w, r, dest, http.StatusSeeOther)
		return
	}
	if r.URL.Path == wire.Prefix+"reauthorize.js" {
		asset(w, r, "reauthorize.js")
		return
	}
	granted, sid := s.access(r, a)
	if !granted {
		switch {
		case r.Method == "GET" && r.URL.Path == wire.Prefix+"shell":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.WriteHeader(http.StatusUnauthorized)
			page(w, "reauthorize.html", nil)
		case r.Method == "GET" && !strings.HasPrefix(r.URL.Path, wire.Prefix):
			dest := "https://" + s.cfg.Domain + "/authorize?" + url.Values{"app": {sub}, "next": {r.URL.RequestURI()}}.Encode()
			http.Redirect(w, r, dest, 303)
		default:
			bad(w, 401, "Application login required")
		}
		return
	}
	switch r.URL.Path {
	case wire.Prefix + "shell":
		shellPage(w, r, a)
	case wire.Prefix + "offer":
		s.offer(w, r, a, sid)
	case wire.Prefix + "shell.js", wire.Prefix + "worker.js", wire.Prefix + "socket.js", wire.Prefix + "style.css":
		if r.URL.Path == wire.Prefix+"worker.js" {
			w.Header().Set("Service-Worker-Allowed", "/")
		}
		asset(w, r, strings.TrimPrefix(r.URL.Path, wire.Prefix))
	default:
		if strings.HasPrefix(r.URL.Path, wire.Prefix) {
			bad(w, 404, "Not found")
			return
		}
		shellPage(w, r, a)
	}
}

func shellPage(w http.ResponseWriter, r *http.Request, a app) {
	if r.Method != "GET" && r.Method != "HEAD" {
		bad(w, 404, "Not found")
		return
	}
	// The shell mirrors the application's route. Don't copy it into the
	// Referer of the shell's assets or signaling requests.
	w.Header().Set("Referrer-Policy", "origin")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; connect-src 'self'; worker-src 'self'; frame-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	settings, _ := json.Marshal(a.browserSettings())
	page(w, "shell.html", string(settings))
}

func asset(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != "GET" && r.Method != "HEAD" {
		bad(w, 405, "Method not allowed")
		return
	}
	b, err := web.ReadFile("web/" + name)
	if err != nil {
		bad(w, 404, "Not found")
		return
	}
	typ := "text/javascript; charset=utf-8"
	if strings.HasSuffix(name, ".css") {
		typ = "text/css; charset=utf-8"
	}
	w.Header().Set("Content-Type", typ)
	if r.Method == "GET" {
		_, _ = w.Write(b)
	}
}

func page(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template %s: %v", name, err)
	}
}
func body(w http.ResponseWriter, r *http.Request, max int64) io.ReadCloser {
	return http.MaxBytesReader(w, r.Body, max)
}

type grant struct {
	App              int64
	SID, Next, Nonce string
	Exp              int64
}

func localPath(p string) string {
	if len(p) > 3000 || len(p) == 0 || p[0] != '/' || strings.HasPrefix(p, "//") ||
		strings.HasPrefix(p, wire.Prefix) || strings.ContainsAny(p, "\\\r\n") {
		return "/"
	}
	u, err := url.ParseRequestURI(p)
	if err != nil || u.Host != "" || strings.HasPrefix(u.Path, "//") || strings.ContainsRune(u.Path, '\\') {
		return "/"
	}
	return p
}
func (s *Server) access(r *http.Request, a app) (bool, string) {
	if a.Public {
		return true, ""
	}
	var g grant
	if !s.verify("app grant", cookie(r, grantCookie), &g) || g.App != a.ID || g.Exp < time.Now().Unix() {
		return false, ""
	}
	u, ok := s.userBySession(g.SID)
	return ok && u.Name == a.Owner, g.SID
}
func (s *Server) acceptGrant(w http.ResponseWriter, r *http.Request, a app) {
	if r.Method != "POST" || r.Header.Get("Origin") != "https://"+s.cfg.Domain {
		bad(w, 403, "Forbidden")
		return
	}
	r.Body = body(w, r, 12<<10)
	if r.ParseForm() != nil {
		bad(w, 400, "Invalid form")
		return
	}
	var g grant
	if !s.verify("handoff", r.PostForm.Get("ticket"), &g) || g.App != a.ID || g.Exp < time.Now().Unix() {
		bad(w, 403, "Login handoff expired")
		return
	}
	u, ok := s.userBySession(g.SID)
	if !ok || u.Name != a.Owner {
		bad(w, 403, "Forbidden")
		return
	}
	_, err := s.db.Exec("INSERT INTO handoffs(nonce,expires) VALUES(?,?)", g.Nonce, g.Exp)
	if err != nil {
		bad(w, 403, "Login handoff already used or unavailable")
		return
	}
	next := localPath(g.Next)
	g.Next, g.Nonce, g.Exp = "", "", time.Now().Add(7*24*time.Hour).Unix()
	secureCookie(w, grantCookie, s.sign("app grant", g), 7*86400)
	http.Redirect(w, r, next, 303)
}

type agent struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	pending map[string]chan wire.Signal
	peers   map[string]livePeer
	done    chan struct{}
}
type livePeer struct {
	id, sid string
	expires time.Time
	app     app
}

func (a *agent) send(v wire.Signal) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_ = a.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return a.conn.WriteJSON(v)
}

func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	if !s.allow("agent:"+s.remote(r), 120, time.Minute) {
		bad(w, 429, "Too many signaling connections")
		return
	}
	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || len(token) != 43 || r.Header.Get("Origin") != "" {
		bad(w, 401, "Unauthorized")
		return
	}
	var id int64
	if s.db.QueryRow(`SELECT c.id FROM clients c JOIN users u ON u.id=c.user_id WHERE c.token_hash=? AND NOT u.disabled`, hash(token)).Scan(&id) != nil {
		bad(w, 401, "Unauthorized")
		return
	}
	up := websocket.Upgrader{HandshakeTimeout: 5 * time.Second}
	c, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	a := &agent{conn: c, pending: map[string]chan wire.Signal{}, peers: map[string]livePeer{}, done: make(chan struct{})}
	s.mu.Lock()
	if s.agents[id] != nil {
		s.mu.Unlock()
		_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1008, "client already connected"), time.Now().Add(time.Second))
		_ = c.Close()
		return
	}
	s.agents[id] = a
	s.mu.Unlock()
	defer func() {
		_ = c.Close()
		s.mu.Lock()
		delete(s.agents, id)
		close(a.done)
		s.mu.Unlock()
	}()
	c.SetReadLimit(256 << 10)
	_ = c.SetReadDeadline(time.Now().Add(45 * time.Second))
	c.SetPongHandler(func(string) error { return c.SetReadDeadline(time.Now().Add(45 * time.Second)) })
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-a.done:
				return
			case <-t.C:
				if c.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					_ = c.Close()
					return
				}
			}
		}
	}()
	for {
		var msg wire.Signal
		if c.ReadJSON(&msg) != nil {
			return
		}
		s.mu.Lock()
		if msg.Closed {
			delete(a.peers, msg.ID)
		}
		if ch := a.pending[msg.ID]; ch != nil {
			select {
			case ch <- msg:
			default:
			}
		}
		s.mu.Unlock()
	}
}

func (s *Server) offer(w http.ResponseWriter, r *http.Request, a app, sid string) {
	if r.Method != "POST" || !sameOrigin(r) {
		bad(w, 403, "Forbidden")
		return
	}
	if !s.allow("offer:"+r.Host+":"+s.remote(r), 100, time.Minute) {
		bad(w, 429, "Too many connections")
		return
	}
	var msg wire.Signal
	if err := json.NewDecoder(body(w, r, 220<<10)).Decode(&msg); err != nil {
		bad(w, 400, "Invalid WebRTC offer")
		return
	}
	var valid bool
	msg.SDP, valid = applicationOffer(msg.SDP, a.Mode == "stun")
	if !valid {
		bad(w, 400, "Invalid WebRTC data-channel offer")
		return
	}
	if msg.Version != wire.Version {
		bad(w, 409, "Protocol changed; reload the application")
		return
	}
	peer := livePeer{id: random(24), sid: sid, expires: time.Now().Add(peerTTL), app: a}
	ice := a.ICE
	if ice == nil {
		ice = []wire.ICE{}
	}
	s.mu.Lock()
	conn := s.agents[a.ClientID]
	if conn == nil {
		s.mu.Unlock()
		bad(w, 503, "Client is offline")
		return
	}
	if len(conn.pending) >= 16 || len(conn.peers) >= 128 {
		s.mu.Unlock()
		bad(w, 429, "Client is busy")
		return
	}
	ch := make(chan wire.Signal, 1)
	conn.pending[peer.id] = ch
	conn.peers[peer.id] = peer
	s.mu.Unlock()
	ok := false
	defer func() {
		s.mu.Lock()
		delete(conn.pending, peer.id)
		if !ok {
			delete(conn.peers, peer.id)
		}
		s.mu.Unlock()
		if !ok {
			_ = conn.send(wire.Signal{ID: peer.id, Closed: true})
		}
	}()
	if err := conn.send(wire.Signal{ID: peer.id, SDP: msg.SDP, Target: a.Target, Origin: "https://" + r.Host, AppID: a.ID, Version: wire.Version, ICE: ice}); err != nil {
		_ = conn.conn.Close()
		bad(w, 503, "Client disconnected")
		return
	}
	timer := time.NewTimer(25 * time.Second)
	defer timer.Stop()
	select {
	case <-r.Context().Done():
		return
	case <-conn.done:
		bad(w, 503, "Client disconnected")
	case <-timer.C:
		bad(w, 504, "Client WebRTC negotiation timed out")
	case answer := <-ch:
		if answer.Error != "" || answer.Closed || answer.SDP == "" || answer.Version != wire.Version {
			bad(w, 502, "Client rejected the WebRTC offer; check Client version and configuration")
			return
		}
		ok = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"sdp": answer.SDP})
	}
}

func applicationOffer(sdp string, direct bool) (string, bool) {
	if !strings.HasPrefix(sdp, "v=0\r\n") && !strings.HasPrefix(sdp, "v=0\n") {
		return "", false
	}
	var out strings.Builder
	media := 0
	for i, line := range strings.Split(sdp, "\n") {
		if strings.HasPrefix(line, "m=") {
			media++
			if !strings.HasPrefix(line, "m=application ") {
				return "", false
			}
		}
		if direct && strings.HasPrefix(line, "a=candidate:") {
			f := strings.Fields(line)
			if len(f) < 8 || f[6] != "typ" {
				return "", false
			}
			if f[7] == "relay" {
				continue
			}
		}
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(line)
	}
	return out.String(), media == 1
}

func (s *Server) maintain(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		type item struct {
			conn *agent
			peer livePeer
		}
		var peers []item
		s.mu.Lock()
		for _, c := range s.agents {
			for _, p := range c.peers {
				peers = append(peers, item{c, p})
			}
		}
		now := time.Now()
		for k, v := range s.limits {
			if now.After(v.until) {
				delete(s.limits, k)
			}
		}
		s.mu.Unlock()
		_, _ = s.db.Exec("DELETE FROM sessions WHERE expires < ?", now.Unix())
		_, _ = s.db.Exec("DELETE FROM handoffs WHERE expires < ?", now.Unix())
		for _, i := range peers {
			p := i.peer
			a, err := s.lookup(p.app.Name, p.app.Owner)
			valid := err == nil && a.ID == p.app.ID && a.Target == p.app.Target && a.ClientID == p.app.ClientID &&
				a.Public == p.app.Public && a.Mode == p.app.Mode && reflect.DeepEqual(a.ICE, p.app.ICE) && p.expires.After(now)
			if valid && !a.Public {
				u, ok := s.userBySession(p.sid)
				valid = ok && u.Name == a.Owner
			}
			if !valid {
				if i.conn.send(wire.Signal{ID: p.id, Closed: true}) != nil {
					_ = i.conn.conn.Close()
				}
				s.mu.Lock()
				delete(i.conn.peers, p.id)
				s.mu.Unlock()
			}
		}
	}
}
