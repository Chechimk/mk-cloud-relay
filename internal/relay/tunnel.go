package relay

import (
	"context"
	"crypto/subtle"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// tunnelHandler accepts an authenticated WebSocket and pipes its binary
// messages to the route's fixed TCP address. This carries protocols that are
// not HTTP (an HTTP proxy's CONNECT, SSH, ...) through Cloud Run, which only
// accepts HTTP requests. The client side is "mk-cloud-relay client" or any
// WebSocket tool that can send an Authorization header.
func (s *Server) tunnelHandler(rt *route) http.Handler {
	tokens := rt.Tokens
	if len(tokens) == 0 {
		tokens = s.cfg.TunnelTokens
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tokenOK(r, tokens) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mk-cloud-relay"`)
			http.Error(w, "401 unauthorized", http.StatusUnauthorized)
			return
		}
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "400 tunnel routes need a WebSocket upgrade over HTTP/1.1 "+
				"(turn off HTTP/2 end-to-end on Cloud Run)", http.StatusBadRequest)
			return
		}
		if max := int64(s.cfg.MaxTunnels); max > 0 && s.tunnels.Load() >= max {
			http.Error(w, "503 too many tunnels", http.StatusServiceUnavailable)
			return
		}
		backend, err := s.dialer().DialContext(r.Context(), "tcp", rt.Addr)
		if err != nil {
			s.log.Warn("tunnel backend error", "route", rt.Prefix, "backend", rt.Addr, "error", err.Error())
			http.Error(w, "502 bad gateway", http.StatusBadGateway)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true, // not a browser endpoint; access is token-based
			CompressionMode:    websocket.CompressionDisabled,
		})
		if err != nil {
			backend.Close()
			return
		}
		s.tunnels.Add(1)
		defer s.tunnels.Add(-1)
		start := time.Now()
		up, down := ServeWebSocketPipe(context.Background(), c, backend, s.cfg.WSPingInterval)
		s.log.Debug("tunnel closed", "route", rt.Prefix, "bytes_up", up, "bytes_down", down,
			"seconds", int(time.Since(start).Seconds()))
	})
}

func tokenOK(r *http.Request, tokens []string) bool {
	got := r.Header.Get("X-Relay-Token")
	if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		got = a[7:]
	}
	if got == "" {
		return false
	}
	ok := 0
	for _, t := range tokens {
		ok |= subtle.ConstantTimeCompare([]byte(got), []byte(t))
	}
	return ok == 1
}

// ServeWebSocketPipe copies bytes between a WebSocket and a TCP connection
// until either side closes, sending WebSocket pings to keep idle mobile and
// NAT paths open. It returns the bytes sent to the backend and to the client.
func ServeWebSocketPipe(ctx context.Context, c *websocket.Conn, tcp net.Conn, ping time.Duration) (int64, int64) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.SetReadLimit(4 << 20)
	ws := websocket.NetConn(ctx, c, websocket.MessageBinary)
	if ping > 0 {
		go func() {
			t := time.NewTicker(ping)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					pctx, pcancel := context.WithTimeout(ctx, 15*time.Second)
					err := c.Ping(pctx)
					pcancel()
					if err != nil {
						cancel()
						ws.Close()
						tcp.Close()
						return
					}
				}
			}
		}()
	}
	// A WebSocket has no half-close, so the session ends when either side
	// finishes.
	return Pipe(ws, tcp, false)
}

// Pipe copies in both directions and returns the bytes copied a->b and b->a.
// With halfClose, the end of one direction is passed on as a TCP half-close
// and the other direction keeps running; otherwise both connections are
// closed as soon as either direction ends.
func Pipe(a, b net.Conn, halfClose bool) (int64, int64) {
	var ab, ba int64
	var once sync.Once
	closeBoth := func() { once.Do(func() { a.Close(); b.Close() }) }
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ab = copyBuf(b, a)
		if !halfClose || !closeWrite(b) {
			closeBoth()
		}
	}()
	go func() {
		defer wg.Done()
		ba = copyBuf(a, b)
		if !halfClose || !closeWrite(a) {
			closeBoth()
		}
	}()
	wg.Wait()
	closeBoth()
	return ab, ba
}

func copyBuf(dst io.Writer, src io.Reader) int64 {
	buf := bufPool.Get()
	defer bufPool.Put(buf)
	n, _ := io.CopyBuffer(dst, src, buf)
	return n
}

func closeWrite(c net.Conn) bool {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite() == nil
	}
	return false
}
