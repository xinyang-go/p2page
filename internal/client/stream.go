package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/datachannel"
	"p2page/internal/wire"
)

type socketClose struct {
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

func handleStream(parent context.Context, rw datachannel.ReadWriteCloserDeadliner, signal wire.Signal, u *upstream, base *url.URL, jar *cookieJar) {
	defer rw.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = rw.Close() })
	defer stop()
	w := &wire.Writer{W: rw}
	fail := func(s string) { _ = w.Send(wire.Failure, []byte(s)) }
	_ = rw.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, wire.Frame)
	n, err := rw.Read(buf)
	if err != nil || n < 2 || buf[0] != wire.Start {
		return
	}
	_ = rw.SetReadDeadline(time.Time{})
	var req wire.Request
	if json.Unmarshal(buf[1:n], &req) != nil {
		fail("Invalid request")
		return
	}
	dest, err := destination(base, req.Path)
	if err != nil {
		fail("Invalid upstream path")
		return
	}
	if req.WS {
		serveSocket(ctx, rw, w, req, signal, u, base, dest, jar)
		return
	}
	serveHTTP(ctx, cancel, rw, w, req, signal, u, base, dest, jar, fail)
}

func proxyHeaders(input http.Header, external, internal string) http.Header {
	h := make(http.Header)
	for k, vs := range input {
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	wire.CleanHeaders(h)
	for _, k := range []string{"Host", "X-Real-IP", "Content-Length", "Cookie", "Cookie2", "Accept-Encoding", "Expect", "Sec-Websocket-Key", "Sec-Websocket-Protocol", "Sec-Websocket-Version", "Sec-Websocket-Extensions"} {
		h.Del(k)
	}
	if h.Get("Origin") == external {
		h.Set("Origin", internal)
	}
	if h.Get("Referer") == external || strings.HasPrefix(h.Get("Referer"), external+"/") {
		h.Set("Referer", internal+strings.TrimPrefix(h.Get("Referer"), external))
	}
	return h
}

func upstreamOrigin(dest *url.URL) string { return dest.Scheme + "://" + dest.Host }

func requestOrigin(u *upstream, dest *url.URL, external string) (origin, host string) {
	if u.publicOriginHeaders {
		if public, err := wire.ParseTarget(external); err == nil && public.Scheme == "https" && public.Path == "" {
			return upstreamOrigin(public), public.Host
		}
	}
	return upstreamOrigin(dest), ""
}

func replyHeaders(status int, h http.Header, base, dest *url.URL) wire.Response {
	h = h.Clone()
	wire.CleanHeaders(h)
	if loc := h.Get("Location"); loc != "" {
		if next, err := dest.Parse(loc); err == nil && wire.Origin(next) == wire.Origin(dest) && next.User == nil {
			escaped := next.EscapedPath()
			prefix := strings.TrimRight(base.EscapedPath(), "/")
			if prefix == "" || escaped == prefix || strings.HasPrefix(escaped, prefix+"/") {
				path := strings.TrimPrefix(escaped, prefix)
				if path == "" {
					path = "/"
				}
				if !strings.HasPrefix(path, "//") {
					if next.RawQuery != "" || next.ForceQuery {
						path += "?" + next.RawQuery
					}
					if next.Fragment != "" {
						path += "#" + next.EscapedFragment()
					}
					h.Set("Location", path)
				}
			}
		}
	}
	h.Del("Set-Cookie")
	h.Del("Set-Cookie2")
	return wire.Response{Status: status, StatusText: http.StatusText(status), Headers: h}
}

func applyResponseCookies(w *wire.Writer, jar *cookieJar, req wire.Request, h http.Header, base *url.URL) error {
	if !req.Credentials {
		return nil
	}
	if lines := h.Values("Set-Cookie"); len(lines) > 0 {
		jar.set(lines, cookieRequestPath(req.Path), strings.TrimRight(base.EscapedPath(), "/"), false, time.Now())
	}
	return sendCookieState(w, jar, req.CookieRevision)
}

var doctype = regexp.MustCompile(`(?is)^((?:\x{FEFF})?\s*<!doctype[^>]*>)`)

const htmlShim = `<script src="/__p2page/socket.js" referrerpolicy="no-referrer"></script>`

func injectHTML(in io.Reader) io.Reader {
	r := bufio.NewReaderSize(in, 4096)
	// Read only the initial doctype (if present). Preserve standards mode.
	b, _ := r.Peek(9)
	n := 0
	if bytes.EqualFold(b, []byte("<!doctype")) || bytes.HasPrefix(b, []byte{0xef, 0xbb, 0xbf}) || (len(b) > 0 && bytes.ContainsRune([]byte(" \t\r\n"), rune(b[0]))) {
		for want := len(b) + 1; want <= 4096; want++ {
			v, err := r.Peek(want)
			if err != nil || v[len(v)-1] == '>' {
				if m := doctype.Find(v); m != nil {
					n = len(m)
				}
				break
			}
		}
	}
	if n == 0 && bytes.HasPrefix(b, []byte{0xef, 0xbb, 0xbf}) {
		n = 3
	}
	prefix := make([]byte, n)
	_, _ = io.ReadFull(r, prefix)
	return io.MultiReader(bytes.NewReader(prefix), strings.NewReader(htmlShim), r)
}

type uploadItem struct {
	data []byte
	end  bool
}

const formBufferLimit = 64 << 10

// Some CGI applications require CONTENT_LENGTH. Measure small form bodies
// and keepalive uploads (including beacons), without buffering large uploads
// or the first part of unrestricted streams.
func upstreamBody(body io.ReadCloser, contentType string, keepalive bool) (io.Reader, error) {
	typ, _, _ := mime.ParseMediaType(contentType)
	if body == nil || (!keepalive && typ != "application/x-www-form-urlencoded" && typ != "multipart/form-data") {
		return body, nil
	}
	prefix, err := io.ReadAll(io.LimitReader(body, formBufferLimit+1))
	if err != nil {
		return nil, err
	}
	buffer := bytes.NewReader(prefix)
	if len(prefix) <= formBufferLimit {
		// net/http uses the size of bytes.Reader as the Content-Length.
		return buffer, nil
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(buffer, body), body}, nil
}

func serveHTTP(ctx context.Context, cancel context.CancelFunc, rw datachannel.ReadWriteCloserDeadliner, w *wire.Writer, req wire.Request, signal wire.Signal, u *upstream, base, dest *url.URL, jar *cookieJar, fail func(string)) {
	switch strings.ToUpper(req.Method) {
	case "CONNECT", "TRACE", "TRACK":
		fail("Method not allowed")
		return
	}
	var body io.ReadCloser
	var pw *io.PipeWriter
	uploads := make(chan uploadItem, wire.Window+1)
	credits := make(chan struct{}, wire.Window)
	if req.Body {
		pr, p := io.Pipe()
		body, pw = pr, p
		defer pr.Close()
		defer pw.Close()
		stop := context.AfterFunc(ctx, func() { _ = pr.CloseWithError(ctx.Err()) })
		defer stop()
		go func() {
			defer pw.Close()
			for {
				select {
				case <-ctx.Done():
					return
				case item := <-uploads:
					if item.end {
						return
					}
					if _, err := pw.Write(item.data); err != nil {
						return
					}
					if w.Send(wire.Credit, []byte{1}) != nil {
						cancel()
						return
					}
				}
			}
		}()
	}
	go func() {
		buf := make([]byte, wire.Frame)
		ended := false
		for {
			n, err := rw.Read(buf)
			if err != nil || n == 0 {
				cancel()
				return
			}
			switch buf[0] {
			case wire.Credit:
				if n != 2 || buf[1] == 0 || int(buf[1]) > wire.Window {
					cancel()
					return
				}
				for i := byte(0); i < buf[1]; i++ {
					select {
					case credits <- struct{}{}:
					default:
						cancel()
						return
					}
				}
			case wire.Data, wire.End:
				if !req.Body || ended || n > wire.Chunk+1 || (buf[0] == wire.End && n != 1) {
					cancel()
					return
				}
				ended = buf[0] == wire.End
				item := uploadItem{end: ended}
				if !ended {
					item.data = append([]byte(nil), buf[1:n]...)
				}
				select {
				case uploads <- item:
				default:
					cancel()
					return
				}
			default:
				cancel()
				return
			}
		}
	}()
	origin, host := requestOrigin(u, dest, signal.Origin)
	headers := proxyHeaders(req.Headers, signal.Origin, origin)
	if req.Body {
		if w.Send(wire.Credit, []byte{wire.Window}) != nil {
			return
		}
	}
	inBody, err := upstreamBody(body, headers.Get("Content-Type"), req.Keepalive)
	if err != nil {
		return
	}
	upReq, err := http.NewRequestWithContext(ctx, req.Method, dest.String(), inBody)
	if err != nil {
		fail("Invalid upstream request")
		return
	}
	upReq.Header = headers
	if host != "" {
		upReq.Host = host
	}
	if req.Credentials {
		if value := jar.header(cookieRequestPath(req.Path), crossSiteEntry(req.EntryOrigin, signal.Origin)); value != "" {
			upReq.Header.Set("Cookie", value)
		}
	}
	resp, err := u.tr.RoundTrip(upReq)
	if err != nil {
		fail("Upstream request failed")
		return
	}
	defer resp.Body.Close()
	if applyResponseCookies(w, jar, req, resp.Header, base) != nil {
		return
	}
	res := replyHeaders(resp.StatusCode, resp.Header, base, dest)
	if _, text, ok := strings.Cut(resp.Status, " "); ok && text != "" {
		res.StatusText = text
	}
	typ, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	disposition, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	transform := !req.Raw && disposition != "attachment" && req.Method != "HEAD" && resp.StatusCode != 204 && resp.StatusCode != 205 &&
		resp.StatusCode != 206 && resp.StatusCode != 304 && typ == "text/html" && res.Headers.Get("Content-Encoding") == ""
	if transform {
		for _, k := range []string{"Content-Length", "ETag", "Content-Md5", "Digest", "Content-Digest", "Accept-Ranges"} {
			res.Headers.Del(k)
		}
		if n := resp.ContentLength; n >= 0 && n <= (1<<63-1)-int64(len(htmlShim)) {
			res.Headers.Set("Content-Length", strconv.FormatInt(n+int64(len(htmlShim)), 10))
		}
	}
	if w.JSON(wire.Headers, res) != nil {
		fail("Upstream response headers are too large")
		return
	}
	var in io.Reader = resp.Body
	if transform {
		in = injectHTML(in)
	}
	buf := make([]byte, wire.Chunk)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			select {
			case <-ctx.Done():
				return
			case <-credits:
			}
			if w.Send(wire.Data, buf[:n]) != nil {
				return
			}
		}
		if err != nil {
			if err == io.EOF {
				_ = w.Send(wire.End, nil)
			} else {
				fail("Upstream response interrupted")
			}
			return
		}
	}
}

func serveSocket(ctx context.Context, rw datachannel.ReadWriteCloserDeadliner, w *wire.Writer, req wire.Request, signal wire.Signal, u *upstream, base, dest *url.URL, jar *cookieJar) {
	origin, host := requestOrigin(u, dest, signal.Origin)
	h := proxyHeaders(req.Headers, signal.Origin, origin)
	h.Set("Origin", origin)
	if host != "" {
		h.Set("Host", host)
	}
	if req.Credentials {
		if value := jar.header(cookieRequestPath(req.Path), false); value != "" {
			h.Set("Cookie", value)
		}
	}
	dial := *u.ws
	dial.Subprotocols = req.Protocols
	socketURL := *dest
	socketURL.Scheme = map[string]string{"http": "ws", "https": "wss"}[dest.Scheme]
	ws, res, err := dial.DialContext(ctx, socketURL.String(), h)
	if err != nil {
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}
		_ = w.Send(wire.Failure, []byte("Upstream WebSocket handshake failed"))
		return
	}
	defer ws.Close()
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()
	ws.SetReadLimit(wire.MaxWS)
	if applyResponseCookies(w, jar, req, res.Header, base) != nil {
		return
	}
	out := replyHeaders(res.StatusCode, res.Header, base, dest)
	out.Protocol = ws.Subprotocol()
	if w.JSON(wire.Headers, out) != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer rw.Close()
		for {
			t, b, err := ws.ReadMessage()
			if err != nil {
				end := socketClose{Code: 1006}
				var e *websocket.CloseError
				if errors.As(err, &e) {
					end.Code, end.Reason = e.Code, e.Text
				}
				_ = w.JSON(wire.Close, end)
				return
			}
			for len(b) > wire.Chunk {
				if w.Send(wire.Data, b[:wire.Chunk]) != nil {
					return
				}
				b = b[wire.Chunk:]
			}
			end := wire.BinaryEnd
			if t == websocket.TextMessage {
				end = wire.TextEnd
			}
			if w.Send(byte(end), b) != nil {
				return
			}
		}
	}()
	defer func() { _ = ws.Close(); _ = rw.Close(); <-done }()
	buf := make([]byte, wire.Frame)
	var payload []byte
	for {
		n, err := rw.Read(buf)
		if err != nil || n == 0 {
			return
		}
		switch buf[0] {
		case wire.Data, wire.TextEnd, wire.BinaryEnd:
			if n > wire.Chunk+1 || len(payload)+n-1 > wire.MaxWS {
				return
			}
			payload = append(payload, buf[1:n]...)
			if buf[0] != wire.Data {
				t := websocket.BinaryMessage
				if buf[0] == wire.TextEnd {
					t = websocket.TextMessage
				}
				_ = ws.SetWriteDeadline(time.Now().Add(20 * time.Second))
				if ws.WriteMessage(t, payload) != nil {
					return
				}
				payload = nil
			}
		case wire.Close:
			var end socketClose
			if json.Unmarshal(buf[1:n], &end) != nil || (end.Code != 1000 && (end.Code < 3000 || end.Code > 4999)) || len(end.Reason) > 123 {
				return
			}
			if ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(end.Code, end.Reason), time.Now().Add(5*time.Second)) != nil {
				return
			}
			select {
			case <-done:
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			return
		default:
			return
		}
	}
}
