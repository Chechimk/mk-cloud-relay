package relay

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// upgradeDrain is how long the relay keeps reading from the client after the
// backend has finished an upgraded connection, so the connection ends with a
// FIN instead of a reset.
const upgradeDrain = 30 * time.Second

// upgradeLinger is how long an upgraded connection stays open toward the
// client after the backend has finished, waiting for the client to close.
var upgradeLinger = 10 * time.Second

// serveUpgrade proxies WebSocket and HTTPUpgrade connections. It is used
// instead of httputil.ReverseProxy's upgrade support so the end of a
// connection can be handled gracefully: when the backend finishes, the
// client gets a TCP half-close (FIN) after the last byte and the relay keeps
// reading what the client still sends. Closing a socket with unread data
// makes the kernel send a reset, and on Cloud Run a reset makes the front end
// drop data it has not yet delivered to a slow client.
func (s *Server) serveUpgrade(w http.ResponseWriter, r *http.Request, rt *route, scheme string, tlsConf *tls.Config) {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL = &url.URL{Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
	out.Body, out.ContentLength = http.NoBody, 0
	out.Header.Del("X-Forwarded-For")
	applyRoute(rt, scheme, r, out)
	for _, h := range []string{"Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Content-Length"} {
		out.Header.Del(h)
	}
	if rt.XForwarded {
		if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			if prior := out.Header.Get("X-Forwarded-For"); prior != "" {
				ip = prior + ", " + ip
			}
			out.Header.Set("X-Forwarded-For", ip)
		}
		out.Header.Set("X-Forwarded-Host", r.Host)
		out.Header.Set("X-Forwarded-Proto", map[bool]string{true: "https", false: "http"}[r.TLS != nil])
	} else {
		out.Header.Del("X-Forwarded-Host")
		out.Header.Del("X-Forwarded-Proto")
		out.Header.Del("Forwarded")
	}
	out.Header.Set("Connection", "Upgrade")

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.DialTimeout)
	conn, err := s.dialer().DialContext(ctx, "tcp", rt.Addr)
	if err == nil && tlsConf != nil {
		c := tlsConf.Clone()
		c.NextProtos = []string{"http/1.1"}
		tc := tls.Client(conn, c)
		if err = tc.HandshakeContext(ctx); err != nil {
			conn.Close()
		}
		conn = tc
	}
	cancel()
	if err != nil {
		s.proxyError(w, r, rt, err)
		return
	}
	_ = conn.SetDeadline(time.Now().Add(s.cfg.DialTimeout + 20*time.Second))
	br := bufio.NewReaderSize(conn, 32<<10)
	var resp *http.Response
	if err = out.Write(conn); err == nil {
		resp, err = http.ReadResponse(br, out)
	}
	if err != nil {
		conn.Close()
		s.proxyError(w, r, rt, err)
		return
	}
	_ = conn.SetDeadline(time.Time{})

	if resp.StatusCode != http.StatusSwitchingProtocols {
		// The backend refused the upgrade: pass its answer through.
		defer conn.Close()
		defer resp.Body.Close()
		for k, vv := range resp.Header {
			if isHopHeader(k) {
				continue
			}
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	client, cbrw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		conn.Close()
		http.Error(w, "500 cannot take over the connection", http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(cbrw, "HTTP/1.1 %s\r\n", resp.Status)
	_ = resp.Header.Write(cbrw)
	_, _ = cbrw.WriteString("\r\n")
	if err := cbrw.Flush(); err != nil {
		client.Close()
		conn.Close()
		return
	}
	start := time.Now()
	st := pipeUpgraded(&bufferedConn{Conn: client, r: cbrw.Reader}, &bufferedConn{Conn: conn, r: br})
	s.log.Debug("upgrade closed", "route", rt.Prefix, "bytes_down", st.down, "down_end", errText(st.downErr),
		"bytes_up", st.up, "up_end", errText(st.upErr), "ms", time.Since(start).Milliseconds())
}

type pipeStats struct {
	up, down       int64
	upErr, downErr error
}

func errText(err error) string {
	if err == nil {
		return "eof"
	}
	return err.Error()
}

// pipeUpgraded copies both ways. When the backend finishes, the client gets
// a half-close and is drained for up to upgradeDrain before the socket is
// closed; when the client finishes first, the backend gets a half-close.
func pipeUpgraded(client, backend net.Conn) pipeStats {
	var st pipeStats
	done := make(chan struct{})
	go func() {
		defer close(done)
		st.up, st.upErr = copyBufErr(backend, client) // ends on client EOF or a backend write error
		closeWrite(backend)
		_, _ = io.Copy(io.Discard, client) // keep reading so close() sends FIN, not RST
	}()
	st.down, st.downErr = copyBufErr(client, backend)
	// Do not close toward the client yet: wait until the client closes
	// (it has then received everything) or the linger time ends.
	_ = client.SetReadDeadline(time.Now().Add(upgradeLinger))
	<-done
	closeWrite(client)
	client.Close()
	backend.Close()
	return st
}

func isHopHeader(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}
