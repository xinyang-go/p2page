package client

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pion/datachannel"
	"golang.org/x/net/publicsuffix"
	"p2page/internal/wire"
)

const (
	maxSessions = 512
	sessionIdle = 7 * 24 * time.Hour
	maxCookies  = 64
	maxJarBytes = 8 << 10 // Also bounds the serialized visible-cookie frame.
)

type sessionKey struct {
	app    int64
	target string
	digest [32]byte
}
type sessionEntry struct {
	jar   *cookieJar
	seen  time.Time
	users int
}
type sessionStore struct {
	mu      sync.Mutex
	entries map[sessionKey]*sessionEntry
}

// The browser capability is only presented over DTLS, and cannot select an
// application or upstream. Both are bound by the authenticated offer.
func (s *sessionStore) acquire(app int64, target, capability string, now time.Time) (*cookieJar, func(), error) {
	secret, err := base64.RawURLEncoding.Strict().DecodeString(capability)
	if len(capability) != 43 || err != nil || len(secret) != 32 {
		return nil, nil, errors.New("invalid browser session")
	}
	key := sessionKey{app: app, target: target, digest: sha256.Sum256(secret)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[sessionKey]*sessionEntry)
	}
	var oldest sessionKey
	var oldestSeen time.Time
	for k, e := range s.entries {
		if e.users != 0 {
			continue
		}
		if now.Sub(e.seen) >= sessionIdle {
			delete(s.entries, k)
		} else if k != key && (oldestSeen.IsZero() || e.seen.Before(oldestSeen)) {
			oldest, oldestSeen = k, e.seen
		}
	}
	e := s.entries[key]
	if e == nil {
		if len(s.entries) >= maxSessions {
			if oldestSeen.IsZero() {
				return nil, nil, errors.New("browser session capacity reached")
			}
			delete(s.entries, oldest)
		}
		e = &sessionEntry{jar: &cookieJar{}}
		s.entries[key] = e
	}
	e.users++
	return e.jar, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		e.users--
		e.seen = time.Now()
	}, nil
}

type cookieJar struct {
	mu       sync.Mutex
	items    []storedCookie
	revision uint64
}
type storedCookie struct {
	http.Cookie
	expires time.Time
}

func pathMatches(request, path string) bool {
	return request == path || (strings.HasPrefix(request, path) &&
		(strings.HasSuffix(path, "/") || request[len(path)] == '/'))
}

func defaultCookiePath(request string) string {
	if i := strings.LastIndexByte(request, '/'); i > 0 {
		return request[:i]
	}
	return "/"
}

func (j *cookieJar) prune(now time.Time) {
	for i := len(j.items) - 1; i >= 0; i-- {
		if exp := j.items[i].expires; !exp.IsZero() && !exp.After(now) {
			j.items = append(j.items[:i], j.items[i+1:]...)
			j.revision++
		}
	}
}

// set normalizes cookies to the application's externally visible path space.
// Domain attributes never cross an app boundary. A script cannot overwrite or
// remove an HttpOnly entry, even if it guesses both its name and its path.
func (j *cookieJar) set(lines []string, requestPath, basePath string, script bool, now time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.prune(now)
	for _, line := range lines {
		if len(line) > 4096 {
			continue
		}
		c, err := http.ParseSetCookie(line)
		if err != nil || strings.HasPrefix(strings.ToLower(c.Name), "__host-p2page-") || (script && c.HttpOnly) {
			continue
		}
		if strings.HasPrefix(c.Name, "__Host-") && (!c.Secure || c.Domain != "" || c.Path != "/") ||
			strings.HasPrefix(c.Name, "__Secure-") && !c.Secure {
			continue
		}
		c.Domain = ""
		if !strings.HasPrefix(c.Path, "/") {
			c.Path = defaultCookiePath(requestPath)
		} else if basePath != "" {
			switch {
			case pathMatches(basePath, c.Path):
				c.Path = "/"
			case strings.HasPrefix(c.Path, basePath+"/"):
				c.Path = strings.TrimPrefix(c.Path, basePath)
			default:
				continue
			}
		}
		if c.Valid() != nil {
			continue
		}
		pos := -1
		for i := range j.items {
			if j.items[i].Name == c.Name && j.items[i].Path == c.Path {
				pos = i
				break
			}
		}
		if pos >= 0 && script && j.items[pos].HttpOnly {
			continue
		}
		exp := c.Expires
		if c.MaxAge > 0 {
			seconds := min(int64(c.MaxAge), int64((400*24*time.Hour)/time.Second))
			exp = now.Add(time.Duration(seconds) * time.Second)
		}
		if c.MaxAge < 0 || (!exp.IsZero() && !exp.After(now)) {
			if pos >= 0 {
				j.items = append(j.items[:pos], j.items[pos+1:]...)
				j.revision++
			}
			continue
		}
		n := cookieCost(c)
		for i := range j.items {
			if i != pos {
				n += cookieCost(&j.items[i].Cookie)
			}
		}
		if n > maxJarBytes || (pos < 0 && len(j.items) >= maxCookies) {
			continue
		}
		entry := storedCookie{Cookie: http.Cookie{
			Name: c.Name, Value: c.Value, Path: c.Path, HttpOnly: c.HttpOnly, Quoted: c.Quoted, SameSite: c.SameSite,
		}, expires: exp}
		if pos < 0 {
			j.items = append(j.items, entry)
		} else {
			j.items[pos] = entry
		}
		j.revision++
	}
}

func cookieCost(c *http.Cookie) int { return len(c.Name) + len(c.Value) + len(c.Path) + 64 }

func (j *cookieJar) sortedItems(now time.Time) []storedCookie {
	j.prune(now)
	items := append([]storedCookie(nil), j.items...)
	sort.SliceStable(items, func(i, k int) bool { return len(items[i].Path) > len(items[k].Path) })
	return items
}

func crossSiteEntry(referrer, origin string) bool {
	if referrer == "" {
		return false
	}
	site := func(value string) string {
		u, err := wire.ParseTarget(value)
		if err != nil || u.Path != "" {
			return ""
		}
		host := strings.ToLower(u.Hostname())
		if net.ParseIP(host) == nil {
			if registrable, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
				host = registrable
			}
		}
		return u.Scheme + "://" + host
	}
	return site(referrer) != site(origin)
}

func (j *cookieJar) header(path string, crossSiteEntry bool) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var pairs []string
	for _, c := range j.sortedItems(time.Now()) {
		if pathMatches(path, c.Path) && !(crossSiteEntry && c.SameSite == http.SameSiteStrictMode) {
			pair := http.Cookie{Name: c.Name, Value: c.Value, Quoted: c.Quoted}
			pairs = append(pairs, pair.String())
		}
	}
	return strings.Join(pairs, "; ")
}

func (j *cookieJar) state(known *uint64) *wire.CookieState {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	j.prune(now)
	if known != nil && j.revision == *known {
		return nil
	}
	s := &wire.CookieState{Revision: j.revision, Cookies: []wire.VisibleCookie{}}
	for _, c := range j.sortedItems(now) {
		if !c.HttpOnly {
			v := wire.VisibleCookie{Name: c.Name, Value: c.Value, Path: c.Path}
			if !c.expires.IsZero() {
				v.Expires = c.expires.UnixMilli()
			}
			s.Cookies = append(s.Cookies, v)
		}
	}
	return s
}

func sendCookieState(w *wire.Writer, j *cookieJar, known uint64) error {
	if state := j.state(&known); state != nil {
		return w.JSON(wire.Cookies, state)
	}
	return nil
}

func serveControl(rw datachannel.ReadWriteCloserDeadliner, msg wire.Signal, target *url.URL, store *sessionStore, ready func(*cookieJar)) {
	buf := make([]byte, 8192)
	_ = rw.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := rw.Read(buf)
	var hello wire.Control
	if err != nil || json.Unmarshal(buf[:n], &hello) != nil || hello.Type != "open" {
		return
	}
	j, release, err := store.acquire(msg.AppID, target.String(), hello.Session, time.Now())
	if err != nil {
		return
	}
	defer release()
	ready(j)
	w := &wire.Writer{W: rw}
	if w.JSON(wire.Cookies, wire.Control{Type: "ready", State: j.state(nil)}) != nil {
		return
	}
	_ = rw.SetReadDeadline(time.Time{})
	for {
		n, err := rw.Read(buf)
		var action wire.Control
		if err != nil || json.Unmarshal(buf[:n], &action) != nil || action.Type != "set" || action.ID == 0 {
			return
		}
		if _, err := destination(target, action.Path); err == nil && len(action.Line) <= 4096 {
			j.set([]string{action.Line}, cookieRequestPath(action.Path), "", true, time.Now())
		}
		if w.JSON(wire.Cookies, wire.Control{Type: "ack", ID: action.ID, State: j.state(nil)}) != nil {
			return
		}
	}
}

func cookieRequestPath(request string) string {
	path, _, _ := strings.Cut(request, "?")
	return path
}
