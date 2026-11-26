package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"p2page/internal/wire"
)

type Config struct {
	Server         string     `json:"server"`
	Token          string     `json:"token"`
	Upstreams      []Upstream `json:"upstreams"`
	AllowedICEURLs *[]string  `json:"allowed_ice_urls,omitempty"`
}
type Upstream struct {
	URL                 string `json:"url"`
	CAFile              string `json:"ca_file,omitempty"`
	InsecureSkipVerify  bool   `json:"insecure_skip_verify,omitempty"`
	PublicOriginHeaders bool   `json:"public_origin_headers,omitempty"`
}
type upstream struct {
	base                *url.URL
	tr                  *http.Transport
	ws                  *websocket.Dialer
	publicOriginHeaders bool
}
type client struct {
	sessions sessionStore
	cfg      Config
	endpoint string
	origins  map[string]*upstream
	ice      map[string]bool
	api      *webrtc.API
	streams  chan struct{}
}

func Run(ctx context.Context, path string) error {
	c, err := load(path)
	if err != nil {
		return err
	}
	defer func() {
		for _, u := range c.origins {
			u.tr.CloseIdleConnections()
		}
	}()
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err = c.connect(ctx)
		if ctx.Err() != nil {
			break
		}
		log.Printf("signaling disconnected: %v; retrying in %v", err, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		} else if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return nil
}

func load(path string) (*client, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	c := &client{origins: map[string]*upstream{}, streams: make(chan struct{}, 256)}
	if err = d.Decode(&c.cfg); err != nil {
		return nil, err
	}
	u, err := wire.ParseTarget(c.cfg.Server)
	if err != nil || u.Scheme != "https" || (u.Path != "" && u.Path != "/") || len(c.cfg.Token) != 43 || len(c.cfg.Upstreams) == 0 {
		return nil, errors.New("configure an HTTPS server, a valid client token and at least one upstream")
	}
	c.endpoint = (&url.URL{Scheme: "wss", Host: u.Host, Path: wire.Prefix + "agent"}).String()
	if c.cfg.AllowedICEURLs != nil {
		c.ice = map[string]bool{}
		for _, u := range *c.cfg.AllowedICEURLs {
			c.ice[u] = true
		}
	}
	for _, entry := range c.cfg.Upstreams {
		u, err := wire.ParseTarget(entry.URL)
		if err != nil || (u.Path != "" && u.Path != "/") {
			return nil, errors.New("client upstream allowlist entries must be HTTP(S) origins without a path")
		}
		key := wire.Origin(u)
		if c.origins[key] != nil {
			return nil, errors.New("duplicate upstream origin")
		}
		if entry.InsecureSkipVerify && entry.CAFile != "" {
			return nil, errors.New("insecure_skip_verify and ca_file are mutually exclusive")
		}
		var tc, wsTLS *tls.Config
		if u.Scheme == "https" {
			// Verification is disabled only for this locally configured upstream,
			// never for the signaling server.
			tc = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: entry.InsecureSkipVerify}
			if entry.CAFile != "" {
				f := entry.CAFile
				if !filepath.IsAbs(f) {
					f = filepath.Join(filepath.Dir(path), f)
				}
				b, err := os.ReadFile(f)
				if err != nil {
					return nil, err
				}
				tc.RootCAs = x509.NewCertPool()
				if !tc.RootCAs.AppendCertsFromPEM(b) {
					return nil, fmt.Errorf("no certificates in %s", f)
				}
			}
			wsTLS = tc.Clone()
		} else if entry.InsecureSkipVerify {
			return nil, errors.New("insecure_skip_verify requires an HTTPS upstream")
		} else if entry.CAFile != "" {
			return nil, errors.New("ca_file requires an HTTPS upstream")
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		c.origins[key] = &upstream{base: u, publicOriginHeaders: entry.PublicOriginHeaders,
			tr: &http.Transport{DialContext: dialer.DialContext, TLSClientConfig: tc, ForceAttemptHTTP2: true,
				TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 35 * time.Second, ExpectContinueTimeout: time.Second,
				IdleConnTimeout: 90 * time.Second, MaxIdleConnsPerHost: 32, MaxConnsPerHost: 128, MaxResponseHeaderBytes: 48 << 10},
			ws: &websocket.Dialer{NetDialContext: dialer.DialContext, TLSClientConfig: wsTLS, HandshakeTimeout: 15 * time.Second},
		}
		if entry.InsecureSkipVerify {
			log.Printf("warning: TLS certificate and hostname verification disabled for upstream %s", u.Host)
		}
	}
	settings := webrtc.SettingEngine{}
	settings.DetachDataChannels()
	settings.EnableDataChannelBlockWrite(true)
	c.api = webrtc.NewAPI(webrtc.WithSettingEngine(settings))
	return c, nil
}

func (c *client) connect(ctx context.Context) error {
	h := http.Header{"Authorization": {"Bearer " + c.cfg.Token}}
	d := *websocket.DefaultDialer
	d.HandshakeTimeout = 10 * time.Second
	ws, res, err := d.DialContext(ctx, c.endpoint, h)
	if err != nil {
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer ws.Close()
	go func() { <-ctx.Done(); _ = ws.Close() }()
	log.Print("connected to signaling server")
	ws.SetReadLimit(256 << 10)
	_ = ws.SetReadDeadline(time.Now().Add(50 * time.Second))
	ws.SetPingHandler(func(p string) error {
		_ = ws.SetReadDeadline(time.Now().Add(50 * time.Second))
		return ws.WriteControl(websocket.PongMessage, []byte(p), time.Now().Add(5*time.Second))
	})
	var writeMu, peersMu sync.Mutex
	peers := map[string]context.CancelFunc{}
	send := func(msg wire.Signal) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if ws.WriteJSON(msg) != nil {
			_ = ws.Close()
		}
	}
	for {
		var msg wire.Signal
		if err := ws.ReadJSON(&msg); err != nil {
			return err
		}
		if len(msg.ID) != 32 {
			continue
		}
		peersMu.Lock()
		if msg.Closed {
			stop := peers[msg.ID]
			peersMu.Unlock()
			if stop != nil {
				stop()
			}
			continue
		}
		if len(peers) >= 128 || peers[msg.ID] != nil {
			peersMu.Unlock()
			send(wire.Signal{ID: msg.ID, Error: "peer limit"})
			continue
		}
		pcCtx, stop := context.WithTimeout(ctx, 2*time.Hour)
		peers[msg.ID] = stop
		peersMu.Unlock()
		go func() {
			defer stop()
			defer func() { peersMu.Lock(); delete(peers, msg.ID); peersMu.Unlock() }()
			err := c.peer(pcCtx, stop, msg, send)
			if err != nil {
				log.Printf("WebRTC offer rejected: %v", err)
				send(wire.Signal{ID: msg.ID, Error: "peer rejected"})
			}
			if ctx.Err() == nil {
				send(wire.Signal{ID: msg.ID, Closed: true})
			}
		}()
	}
}

func (c *client) peer(ctx context.Context, stop context.CancelFunc, msg wire.Signal, send func(wire.Signal)) error {
	target, err := wire.ParseTarget(msg.Target)
	if err != nil || msg.AppID <= 0 || msg.Version != wire.Version {
		return errors.New("invalid peer configuration")
	}
	u := c.origins[wire.Origin(target)]
	if u == nil {
		return errors.New("configured target is not on the upstream allowlist")
	}
	// Use the locally configured authority even when alternate spellings match the allowlist.
	target.Host = u.base.Host
	var ice []webrtc.ICEServer
	for _, s := range msg.ICE {
		for _, uri := range s.URLs {
			if c.ice != nil && !c.ice[uri] {
				return errors.New("ICE URL is not on the client allowlist")
			}
		}
		ice = append(ice, webrtc.ICEServer{URLs: s.URLs, Username: s.Username, Credential: s.Credential})
	}
	pc, err := c.api.NewPeerConnection(webrtc.Configuration{ICEServers: ice})
	if err != nil {
		return err
	}
	defer pc.Close()
	ready := make(chan struct{})
	var once sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			once.Do(func() { close(ready) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			stop()
		}
	})
	perPeer := make(chan struct{}, 64)
	var controlSeen atomic.Bool
	var activeJar atomic.Pointer[cookieJar]
	pc.OnDataChannel(func(d *webrtc.DataChannel) {
		d.OnOpen(func() {
			label := d.Label()
			if (label != "control" && label != "request") || !d.Ordered() || d.MaxRetransmits() != nil || d.MaxPacketLifeTime() != nil {
				_ = d.Close()
				return
			}
			if label == "control" && !controlSeen.CompareAndSwap(false, true) {
				_ = d.Close()
				return
			}
			rw, err := d.DetachWithDeadline()
			if err != nil {
				_ = d.Close()
				return
			}
			if label == "control" {
				defer rw.Close()
				defer stop()
				serveControl(rw, msg, target, &c.sessions, activeJar.Store)
				return
			}
			jar := activeJar.Load()
			if jar == nil {
				_ = rw.Close()
				return
			}
			select {
			case perPeer <- struct{}{}:
			default:
				_ = rw.Close()
				return
			}
			defer func() { <-perPeer }()
			select {
			case c.streams <- struct{}{}:
			default:
				_ = rw.Close()
				return
			}
			defer func() { <-c.streams }()
			handleStream(ctx, rw, msg, u, target, jar)
		})
	})
	if err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: msg.SDP}); err != nil {
		return err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(answer); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(20 * time.Second):
		return errors.New("ICE gathering timed out")
	case <-gather:
	}
	send(wire.Signal{ID: msg.ID, SDP: pc.LocalDescription().SDP, Version: wire.Version})
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(35 * time.Second):
		return nil
	case <-ready:
	}
	<-ctx.Done()
	return nil
}

func destination(base *url.URL, path string) (*url.URL, error) {
	r, err := url.ParseRequestURI(path)
	if err != nil || r.IsAbs() || r.Host != "" || !strings.HasPrefix(path, "/") || strings.HasPrefix(r.Path, "//") ||
		strings.ContainsRune(r.Path, '\\') || strings.ContainsAny(r.Path, "\r\n\x00") || strings.HasPrefix(r.Path, wire.Prefix) {
		return nil, errors.New("invalid path")
	}
	// Reject dot segments rather than allowing an upstream router to escape a base path.
	for _, s := range strings.Split(r.Path, "/") {
		if s == "." || s == ".." {
			return nil, errors.New("invalid path segment")
		}
	}
	u := *base
	u.Path = strings.TrimRight(base.Path, "/") + r.Path
	u.RawPath = strings.TrimRight(base.EscapedPath(), "/") + r.EscapedPath()
	u.RawQuery, u.ForceQuery = r.RawQuery, r.ForceQuery
	return &u, nil
}
