// Package relay implements the MK Cloud Relay HTTP server.
//
// The relay is protocol-agnostic: it forwards HTTP requests, HTTP/2 streams,
// gRPC, WebSocket/HTTPUpgrade connections and authenticated TCP tunnels
// without reading or changing their payloads.
package relay

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Chechimk/mk-cloud-relay/internal/config"
)

// Server routes incoming requests to their configured backends.
type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	routes  []*route
	fwd     *forwarder
	tunnels atomic.Int64
}

type route struct {
	config.Route
	handler http.Handler
}

// New prepares the handlers for every configured route.
func New(cfg *config.Config, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, log: log}
	for _, rc := range cfg.Routes {
		r := &route{Route: rc}
		if rc.Kind == config.KindTunnel {
			r.handler = s.tunnelHandler(r)
		} else {
			r.handler = s.reverseProxy(r)
		}
		s.routes = append(s.routes, r)
	}
	if cfg.ForwardUpstream != nil {
		s.fwd = newForwarder(s)
	}
	return s
}

// HTTPServer returns an http.Server that accepts HTTP/1.1 and HTTP/2
// cleartext (h2c) on the same port, which is what Cloud Run sends in both of
// its modes.
func (s *Server) HTTPServer(addr string) *http.Server {
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return &http.Server{
		Addr:              addr,
		Handler:           s,
		Protocols:         &p,
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		IdleTimeout:       s.cfg.IdleTimeout,
		MaxHeaderBytes:    64 << 10,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams: int(s.cfg.MaxConcurrentH2),
			SendPingTimeout:      30 * time.Second,
			PingTimeout:          15 * time.Second,
		},
		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
}

// ServeHTTP is the single entry point for all requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w}
	if r.ProtoMajor == 1 {
		// XHTTP stream-one and similar transports read the response while
		// still uploading the request body.
		_ = http.NewResponseController(w).EnableFullDuplex()
	}
	name := "-"
	defer func() {
		if s.cfg.LogRequests {
			// The full path is not logged: it often carries secrets such as
			// gRPC service names or XHTTP session IDs.
			s.log.Info("request", "route", name, "method", r.Method, "proto", r.Proto,
				"status", sw.code(), "bytes_out", sw.n, "ms", time.Since(start).Milliseconds())
		}
	}()

	switch {
	case s.fwd != nil && (r.Method == http.MethodConnect || isAbsoluteForm(r)):
		name = "forward-proxy"
		s.forward(sw, r)
		return
	case r.Method == http.MethodConnect:
		http.Error(sw, "405 method not allowed", http.StatusMethodNotAllowed)
		return
	case s.cfg.HealthPath != "" && r.URL.Path == s.cfg.HealthPath:
		name = "health"
		sw.Header().Set("Content-Type", "text/plain")
		_, _ = sw.Write([]byte("ok\n"))
		return
	}
	rt := s.match(r.URL.Path)
	if rt == nil {
		notFound(sw)
		return
	}
	name = rt.Prefix
	rt.handler.ServeHTTP(sw, r)
}

func (s *Server) match(path string) *route {
	for _, r := range s.routes { // sorted longest prefix first
		if strings.HasPrefix(path, r.Prefix) {
			return r
		}
	}
	return nil
}

func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("404 page not found\n"))
}

func isAbsoluteForm(r *http.Request) bool {
	return r.ProtoMajor == 1 && r.URL.IsAbs() && strings.HasPrefix(r.RequestURI, "http")
}

func isGRPC(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

// statusWriter records the status and size for logging. It forwards
// flushing and hijacking so streaming and upgrades keep working.
type statusWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 || code < 200 { // allow 1xx before the final status
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

func (w *statusWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
