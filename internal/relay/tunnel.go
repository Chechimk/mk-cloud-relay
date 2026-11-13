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
		up, down := serveTunnel(c, backend, s.cfg.WSPingInterval)
		s.log.Debug("tunnel closed", "route", rt.Prefix, "bytes_up", up, "bytes_down", down,
			"seconds", int(time.Since(start).Seconds()))
	})
}

// TunnelEOF is the text message the relay sends after the last byte from the
// backend. Data travels in binary messages, so the marker cannot be confused
// with data; the client closes once it reads it, which tells the relay that
// everything was delivered.
const TunnelEOF = "mk-cloud-relay:eof"

// serveTunnel is the relay side of a tunnel. When the backend finishes, the
// relay sends TunnelEOF and waits for the client to close instead of closing
// first: Cloud Run's front end drops data it has not delivered yet when the
// container closes first, and a WebSocket close handshake times out after a
// few seconds, which a busy link can exceed.
func serveTunnel(c *websocket.Conn, tcp net.Conn, ping time.Duration) (up, down int64) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.SetReadLimit(4 << 20)
	ws := websocket.NetConn(ctx, c, websocket.MessageBinary)
	startPings(ctx, c, ping, func() { cancel(); ws.Close(); tcp.Close() })
	upDone := make(chan struct{})
	go func() {
		defer close(upDone)
		up = copyBuf(tcp, ws) // ends when the client closes
		closeWrite(tcp)
	}()
	down = copyBuf(ws, tcp) // ends when the backend closes
	wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	_ = c.Write(wctx, websocket.MessageText, []byte(TunnelEOF))
	wcancel()
	select {
	case <-upDone:
	case <-time.After(wsCloseWait):
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	tcp.Close()
	<-upDone
	return up, down
}

func startPings(ctx context.Context, c *websocket.Conn, every time.Duration, onFail func()) {
	if every <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(every)
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
					onFail()
					return
				}
			}
		}
	}()
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

// ServeWebSocketPipe is the client side of a tunnel: it copies bytes between
// the WebSocket and a local TCP connection until either side finishes,
// sending pings to keep idle mobile and NAT paths open. Reading the relay's
// TunnelEOF text message ends the WebSocket-to-TCP direction (after all data
// has been written), which closes both sides and so tells the relay that
// everything arrived. It returns the bytes sent to the relay and received.
func ServeWebSocketPipe(ctx context.Context, c *websocket.Conn, tcp net.Conn, ping time.Duration) (int64, int64) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.SetReadLimit(4 << 20)
	ws := websocket.NetConn(ctx, c, websocket.MessageBinary)
	startPings(ctx, c, ping, func() { cancel(); ws.Close(); tcp.Close() })
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
	n, _ := copyBufErr(dst, src)
	return n
}

func copyBufErr(dst io.Writer, src io.Reader) (int64, error) {
	buf := bufPool.Get()
	defer bufPool.Put(buf)
	return io.CopyBuffer(dst, src, buf)
}

func closeWrite(c net.Conn) bool {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite() == nil
	}
	return false
}
