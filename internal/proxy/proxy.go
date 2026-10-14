package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"golang.org/x/net/proxy"

	"lanvello/internal/lanes"
)

// Proxy is a local HTTP(S) proxy whose every byte leaves through a lane.
//
// It terminates the client's TLS with its own CA, so a 429 from the free tier
// is visible as a status instead of an opaque stream: the exit is retired until
// retry-after and the very same request is replayed on the next lane. That is
// the whole trick — the client keeps its session lease, the throttle moves.
type Proxy struct {
	Lanes *lanes.Manager
	CA    *CA
	// Attempts bounds the lane rotation for one client request.
	Attempts int
	// Logf reports lane decisions (429 and free-tier rotations among them).
	Logf func(string, ...any)

	// refusalPark is how long an exit the free tier refuses stays out of
	// rotation. The far end names no window for that refusal, so pick one.
	refusalPark time.Duration
	// laneWait bounds how long a request waits for a rotating lane to come back
	// with a fresh exit before it settles for the answer it already has.
	laneWait time.Duration

	// dial bounds the connect+TLS+headers phase; the body is not on a clock
	// because a thinking model is not a broken socket.
	dial time.Duration
}

func New(m *lanes.Manager, ca *CA) *Proxy {
	return &Proxy{
		Lanes: m, CA: ca, Attempts: 4,
		dial: 3 * time.Minute, refusalPark: 15 * time.Minute, laneWait: 30 * time.Second,
	}
}

const maxBody = 64 << 20

func (p *Proxy) logf(format string, a ...any) {
	if p.Logf != nil {
		p.Logf(format, a...)
	}
}

// Serve accepts client connections until the listener dies.
func (p *Proxy) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go p.conn(c)
	}
}

func (p *Proxy) conn(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if req.Method == http.MethodConnect {
			p.connect(c, req)
			return
		}
		if !p.relay(c, req, "http", req.Host) {
			return
		}
		if req.Close {
			return
		}
	}
}

// connect upgrades the client to TLS with a minted certificate for the target
// host and then serves whatever rides on that tunnel — normally opencode's
// whole session, one request after another.
func (p *Proxy) connect(client net.Conn, req *http.Request) {
	host, port := splitHostPort(req.Host, "443")
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	cert, err := p.CA.Cert(host)
	if err != nil {
		p.logf("cert %s: %v", host, err)
		return
	}
	tc := tls.Server(client, &tls.Config{Certificates: []tls.Certificate{cert}})
	if err := tc.Handshake(); err != nil {
		p.logf("tls handshake %s: %v", host, err)
		return
	}
	defer tc.Close()
	tbr := bufio.NewReader(tc)
	for {
		creq, err := http.ReadRequest(tbr)
		if err != nil {
			return
		}
		if !p.relay(tc, creq, "https", net.JoinHostPort(host, port)) {
			return
		}
		if creq.Close {
			return
		}
	}
}

// relay performs one client request against the lane pool and writes the
// answer back. It returns false when the client connection is done for.
func (p *Proxy) relay(w io.Writer, req *http.Request, scheme, addr string) bool {
	host, port := splitHostPort(addr, defaultPort(scheme))
	body, err := readBody(req)
	if err != nil {
		writeStatus(w, http.StatusRequestEntityTooLarge, "request body too large")
		return false
	}
	resp, lane, err := p.roundtrip(scheme, host, port, req, body)
	if err != nil {
		p.logf("%s %s%s: %v", req.Method, host, req.URL.Path, err)
		writeStatus(w, http.StatusBadGateway, err.Error())
		return false
	}
	defer resp.Body.Close()
	if err := p.writeResponse(w, resp); err != nil {
		return false
	}
	p.logf("%d %s %s%s lane %s{%s}", resp.StatusCode, req.Method, host, req.URL.Path, laneTag(lane), laneCountry(lane))
	return true
}

// roundtrip sends the request on a lane and rotates on a throttled exit. The
// client sees one answer: retries happen before a single byte goes downstream.
func (p *Proxy) roundtrip(scheme, host, port string, req *http.Request, body []byte) (*http.Response, *lanes.Lane, error) {
	tried := map[int]bool{}
	var last *http.Response
	var lastErr error
	deadline := time.Now().Add(p.laneWait)
	for attempts := 0; attempts < p.Attempts; {
		l := p.Lanes.Pick(tried)
		if l == nil {
			// Every lane is rotating or parked — and a rotation is exactly what
			// this request is waiting for, since the fresh exit is the answer.
			// Wait for one, then give the new exits another look.
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(200 * time.Millisecond)
			tried = map[int]bool{}
			continue
		}
		attempts++
		tried[l.Index] = true
		started := time.Now()
		resp, err := p.attempt(l, scheme, host, port, req, body)
		if err != nil {
			lastErr = err
			p.Lanes.NoteResult(l.Country, 502)
			p.Lanes.Release(l)
			continue
		}
		p.Lanes.Emit(lanes.Proof{T: time.Now(), Lane: l.Index, Country: l.Country, IP: l.ExitIP,
			Model: host + req.URL.Path, Status: resp.StatusCode, Bytes: resp.ContentLength, Secs: time.Since(started).Seconds()})
		if freeTierRefused(resp) {
			// The far end has had enough of this exit: the same answer as a
			// 429, and the same remedy — park it, replay elsewhere, and let the
			// next request start on an exit that still passes.
			p.Lanes.NoteLimited(l, p.refusalPark)
			country := p.Lanes.Rotate(l)
			p.logf("%s %s%s: lane %d{%s} refused the free tier -- replaying on %s",
				req.Method, host, req.URL.Path, l.Index, l.Country, country)
			drain(resp)
			p.Lanes.Release(l)
			last = &http.Response{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: http.NoBody, Request: req}
			lastErr = fmt.Errorf("lane %d{%s} refused the free tier", l.Index, l.Country)
			continue
		}
		if !throttled(resp.StatusCode) {
			p.Lanes.NoteResult(l.Country, resp.StatusCode)
			p.Lanes.Release(l)
			return resp, l, nil
		}
		// The exit is spent: park it for as long as the far end said, ask for a
		// fresh one in the best country and put this request elsewhere.
		d := retryAfter(resp)
		p.Lanes.NoteLimited(l, d)
		country := p.Lanes.Rotate(l)
		p.logf("%s %s%s: lane %d{%s} %d exit limited %s -- replaying on %s",
			req.Method, host, req.URL.Path, l.Index, l.Country, resp.StatusCode, d, country)
		drain(resp)
		p.Lanes.Release(l)
		last = &http.Response{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: http.NoBody, Request: req}
		lastErr = fmt.Errorf("lane %d{%s} answered %d", l.Index, l.Country, resp.StatusCode)
	}
	if last != nil {
		return last, nil, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no lane answered")
	}
	return nil, nil, lastErr
}

// attempt dials one lane, speaks TLS to the far end and returns the response
// headers with the body still streaming.
func (p *Proxy) attempt(l *lanes.Lane, scheme, host, port string, req *http.Request, body []byte) (*http.Response, error) {
	conn, err := p.dialLane(l, host, port)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(p.dial)
	_ = conn.SetDeadline(deadline)

	var rw io.ReadWriter = conn
	if scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: host})
		if err := tc.Handshake(); err != nil {
			conn.Close()
			return nil, err
		}
		rw = tc
	}

	out := req.Clone(context.Background())
	out.URL.Scheme, out.URL.Host = scheme, net.JoinHostPort(host, port)
	out.RequestURI = ""
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	out.Close = true
	if req.Host != "" {
		out.Host = req.Host
	}
	cleanHeaders(out.Header)
	if err := out.Write(rw); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(rw)
	resp, err := http.ReadResponse(br, out)
	if err != nil {
		conn.Close()
		return nil, err
	}
	// Streaming starts now: a model that thinks for minutes must not be cut.
	_ = conn.SetDeadline(time.Time{})
	resp.Body = &laneBody{ReadCloser: resp.Body, conn: conn}
	return resp, nil
}

func (p *Proxy) dialLane(l *lanes.Lane, host, port string) (net.Conn, error) {
	addr := net.JoinHostPort(host, port)
	if l == nil || l.SocksAddr == "" || isLoopback(host) {
		// The client's own local server is not lane material.
		return net.DialTimeout("tcp", addr, 30*time.Second)
	}
	d, err := proxy.SOCKS5("tcp", l.SocksAddr, nil, proxy.Direct)
	if err != nil {
		return nil, err
	}
	if cd, ok := d.(proxy.ContextDialer); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		return cd.DialContext(ctx, "tcp", addr)
	}
	return d.Dial("tcp", addr)
}

// writeResponse frames the answer for the client without giving up keep-alive:
// the pool may close the upstream connection between requests, the client must
// not have to reconnect its session for that.
func (p *Proxy) writeResponse(w io.Writer, resp *http.Response) error {
	h := resp.Header.Clone()
	h.Del("Connection")
	h.Del("Keep-Alive")
	h.Del("Proxy-Connection")
	h.Del("Transfer-Encoding")
	h.Del("Upgrade")
	h.Del("Content-Length")

	var head bytes.Buffer
	fmt.Fprintf(&head, "HTTP/1.1 %d %s\r\n", resp.StatusCode, statusText(resp.StatusCode))
	if resp.ContentLength >= 0 {
		h.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	} else {
		h.Set("Transfer-Encoding", "chunked")
	}
	for k, vs := range h {
		for _, v := range vs {
			fmt.Fprintf(&head, "%s: %s\r\n", k, v)
		}
	}
	head.WriteString("\r\n")
	if _, err := w.Write(head.Bytes()); err != nil {
		return err
	}
	if resp.ContentLength >= 0 {
		_, err := io.Copy(w, resp.Body)
		return err
	}
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := fmt.Fprintf(w, "%x\r\n", n); werr != nil {
				return werr
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if _, werr := io.WriteString(w, "\r\n"); werr != nil {
				return werr
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				_, werr := io.WriteString(w, "0\r\n\r\n")
				return werr
			}
			return rerr
		}
	}
}

// laneBody closes the upstream connection once the answer has been read, which
// keeps one request per lane connection without leaking sockets.
type laneBody struct {
	io.ReadCloser
	conn net.Conn
}

func (b *laneBody) Close() error {
	err := b.ReadCloser.Close()
	b.conn.Close()
	return err
}

// drain reads a bounded slice of an answer we are throwing away so the lane
// connection closes cleanly instead of resetting.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

// refusalPeek is how much of a 403 body is read to tell a free-tier refusal
// from any other 403. The far end's error page is far shorter than this.
const refusalPeek = 8 << 10

// peekedBody keeps the unread rest of a body readable and still closes the
// original: what was peeked is consumed, what follows is not.
type peekedBody struct {
	io.Reader
	io.Closer
}

// freeTierRefused reports whether this 403 is the free tier refusing the exit
// that carried it ("OpenCode's free tier can only be used from within
// OpenCode"), leaving the body readable either way. The far end states the
// refusal in a compressed body — br, as the client asks for — so the peek is
// decoded before it is read.
func freeTierRefused(resp *http.Response) bool {
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, refusalPeek))
	resp.Body = peekedBody{Reader: io.MultiReader(bytes.NewReader(head), resp.Body), Closer: resp.Body}
	if len(head) == 0 && err != nil {
		return false
	}
	plain := head
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "br":
		if b, err := io.ReadAll(io.LimitReader(brotli.NewReader(bytes.NewReader(head)), refusalPeek)); err == nil {
			plain = b
		}
	case "gzip":
		if zr, err := gzip.NewReader(bytes.NewReader(head)); err == nil {
			if b, err := io.ReadAll(io.LimitReader(zr, refusalPeek)); err == nil {
				plain = b
			}
		}
	}
	return bytes.Contains(plain, []byte("FreeTierError")) || bytes.Contains(plain, []byte("free tier can only be used"))
}

func throttled(status int) bool {
	switch status {
	case 429, 502, 503, 504:
		return true
	}
	return false
}

// retryAfter reads the far end's own reset window; 10 minutes is the floor when
// it stays silent, because a spent exit is worth seconds, not another request.
func retryAfter(resp *http.Response) time.Duration {
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	return 10 * time.Minute
}

func readBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	defer req.Body.Close()
	lr := io.LimitReader(req.Body, maxBody+1)
	b, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if len(b) > maxBody {
		return nil, fmt.Errorf("body over %d bytes", maxBody)
	}
	return b, nil
}

func cleanHeaders(h http.Header) {
	for _, k := range []string{"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
}

func writeStatus(w io.Writer, code int, msg string) {
	body := msg + "\n"
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, statusText(code), len(body), body)
}

func statusText(code int) string {
	if t := http.StatusText(code); t != "" {
		return t
	}
	return "Status"
}

func splitHostPort(hostport, defPort string) (string, string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return h, p
	}
	return hostport, defPort
}

func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

func isLoopback(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
}

func laneTag(l *lanes.Lane) string {
	if l == nil {
		return "-"
	}
	return strconv.Itoa(l.Index)
}

func laneCountry(l *lanes.Lane) string {
	if l == nil {
		return "-"
	}
	return l.Country
}
