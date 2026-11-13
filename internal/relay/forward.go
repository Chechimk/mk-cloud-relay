package relay

import (
	"bufio"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// forwarder handles standard HTTP forward-proxy requests (CONNECT and
// absolute-form "GET http://host/..."). It never connects to the destination
// itself: every request goes through the operator's FORWARD_PROXY_UPSTREAM,
// and every client must authenticate with FORWARD_PROXY_USERS.
//
// Google Cloud Run rejects CONNECT at its front end, so on Cloud Run this
// handler only sees plain-HTTP absolute-form requests (if the front end
// passes them at all). For full HTTP+HTTPS proxying through Cloud Run, use a
// tunnel:// route to the same upstream proxy with "mk-cloud-relay client".
type forwarder struct {
	s        *Server
	upstream *url.URL
	authHdr  string // Proxy-Authorization for the upstream, if it has credentials
	httpRP   *httputil.ReverseProxy
}

func newForwarder(s *Server) *forwarder {
	up := s.cfg.ForwardUpstream
	f := &forwarder{s: s, upstream: up}
	if up.User != nil {
		pass, _ := up.User.Password()
		f.authHdr = "Basic " + base64.StdEncoding.EncodeToString([]byte(up.User.Username()+":"+pass))
	}
	t := s.h1Transport()
	t.Proxy = http.ProxyURL(up) // Go adds the upstream's Proxy-Authorization itself
	f.httpRP = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = pr.In.URL
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del("Proxy-Authorization")
		},
		Transport:     t,
		FlushInterval: -1,
		BufferPool:    bufPool,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				s.log.Warn("forward proxy upstream error", "error", err.Error())
				http.Error(w, "502 bad gateway", http.StatusBadGateway)
			}
		},
	}
	return f
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request) {
	f := s.fwd
	if !f.authorized(r) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="mk-cloud-relay"`)
		http.Error(w, "407 proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	if r.Method != http.MethodConnect {
		if r.URL.Scheme != "http" {
			http.Error(w, "400 only http:// URLs can be forwarded; use CONNECT for https", http.StatusBadRequest)
			return
		}
		f.httpRP.ServeHTTP(w, r)
		return
	}
	if r.ProtoMajor != 1 {
		http.Error(w, "405 CONNECT is only supported over HTTP/1.1", http.StatusMethodNotAllowed)
		return
	}
	f.connect(w, r)
}

func (f *forwarder) authorized(r *http.Request) bool {
	h := r.Header.Get("Proxy-Authorization")
	if len(h) < 6 || !strings.EqualFold(h[:6], "basic ") {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[6:]))
	if err != nil {
		return false
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return false
	}
	want, known := f.s.cfg.ForwardUsers[user]
	if !known {
		want = "\x00invalid" // keep the comparison time similar
	}
	return subtle.ConstantTimeCompare([]byte(pass), []byte(want)) == 1 && known
}

// connect opens a CONNECT tunnel through the upstream proxy and then copies
// bytes in both directions. The upstream's own reply (normally "200
// Connection established") is passed back to the client unchanged.
func (f *forwarder) connect(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		http.Error(w, "400 CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	up, err := f.s.dialer().DialContext(r.Context(), "tcp", f.upstream.Host)
	if err != nil {
		f.s.log.Warn("forward proxy upstream error", "error", err.Error())
		http.Error(w, "502 bad gateway", http.StatusBadGateway)
		return
	}
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if f.authHdr != "" {
		req += "Proxy-Authorization: " + f.authHdr + "\r\n"
	}
	req += "\r\n"
	_ = up.SetWriteDeadline(time.Now().Add(f.s.cfg.DialTimeout))
	if _, err := io.WriteString(up, req); err != nil {
		up.Close()
		http.Error(w, "502 bad gateway", http.StatusBadGateway)
		return
	}
	_ = up.SetWriteDeadline(time.Time{})
	client, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		up.Close()
		http.Error(w, "500 cannot take over the connection", http.StatusInternalServerError)
		return
	}
	// bufferedConn first returns bytes the client already sent after its
	// CONNECT request (for example a TLS ClientHello), so none are lost.
	Pipe(&bufferedConn{Conn: client, r: brw.Reader}, up, true)
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c.r.Buffered() > 0 {
		return c.r.Read(p)
	}
	return c.Conn.Read(p)
}

func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
