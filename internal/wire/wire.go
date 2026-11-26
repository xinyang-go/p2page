package wire

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	Start byte = iota + 1
	Data
	End
	Headers
	Failure
	Credit
	TextEnd
	BinaryEnd
	Close
	Cookies

	Chunk   = 32 * 1024
	Frame   = 60 * 1024
	Window  = 32
	MaxWS   = 16 * 1024 * 1024
	Prefix  = "/__p2page/"
	Version = 2
)

type ICE struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type Signal struct {
	ID      string `json:"id"`
	SDP     string `json:"sdp,omitempty"`
	Target  string `json:"target,omitempty"`
	Origin  string `json:"origin,omitempty"`
	AppID   int64  `json:"appId,omitempty"`
	Version int    `json:"version,omitempty"`
	ICE     []ICE  `json:"ice,omitempty"`
	Error   string `json:"error,omitempty"`
	Closed  bool   `json:"closed,omitempty"`
}

type Request struct {
	Method         string      `json:"method"`
	Path           string      `json:"path"`
	Headers        http.Header `json:"headers"`
	Credentials    bool        `json:"credentials"`
	CookieRevision uint64      `json:"cookieRevision,omitempty"`
	Body           bool        `json:"body"`
	Raw            bool        `json:"raw,omitempty"`
	Keepalive      bool        `json:"keepalive,omitempty"`
	EntryOrigin    string      `json:"entryOrigin,omitempty"`
	WS             bool        `json:"ws"`
	Protocols      []string    `json:"protocols"`
}

type Response struct {
	Status     int         `json:"status"`
	StatusText string      `json:"statusText,omitempty"`
	Headers    http.Header `json:"headers,omitempty"`
	Protocol   string      `json:"protocol,omitempty"`
}

type VisibleCookie struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Path    string `json:"path"`
	Expires int64  `json:"expires,omitempty"` // Unix milliseconds; zero for a session cookie.
}

type CookieState struct {
	Revision uint64          `json:"revision"`
	Cookies  []VisibleCookie `json:"cookies"`
}

// Control messages only travel on the encrypted WebRTC control channel.
type Control struct {
	Type    string       `json:"type"`
	Session string       `json:"session,omitempty"`
	ID      uint32       `json:"id,omitempty"`
	Path    string       `json:"path,omitempty"`
	Line    string       `json:"line,omitempty"`
	State   *CookieState `json:"state,omitempty"`
}

func ParseTarget(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("upstream must be an HTTP(S) base URL without credentials, query, or fragment")
	}
	return u, nil
}

func Origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return u.Scheme + "|" + strings.ToLower(u.Hostname()) + "|" + port
}

func CleanHeaders(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, k := range strings.Split(v, ",") {
			h.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
	for k := range h {
		s := strings.ToLower(k)
		if s == "forwarded" || strings.HasPrefix(s, "x-forwarded-") || strings.HasPrefix(s, "x-p2page-") {
			h.Del(k)
		}
	}
}

type Writer struct {
	W  io.Writer
	mu sync.Mutex
}

func (w *Writer) Send(t byte, payload []byte) error {
	if len(payload) >= Frame {
		return errors.New("frame too large")
	}
	frame := make([]byte, 1+len(payload))
	frame[0] = t
	copy(frame[1:], payload)
	w.mu.Lock()
	defer w.mu.Unlock()
	if d, ok := w.W.(interface{ SetWriteDeadline(time.Time) error }); ok {
		if err := d.SetWriteDeadline(time.Now().Add(20 * time.Second)); err != nil {
			return err
		}
	}
	n, err := w.W.Write(frame)
	if err == nil && n != len(frame) {
		return io.ErrShortWrite
	}
	return err
}

func (w *Writer) JSON(t byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.Send(t, b)
}
