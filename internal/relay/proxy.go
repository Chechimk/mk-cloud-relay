package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/Chechimk/mk-cloud-relay/internal/config"
)

var bufPool = &byteBufferPool{p: sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}}

type byteBufferPool struct{ p sync.Pool }

func (b *byteBufferPool) Get() []byte  { return *(b.p.Get().(*[]byte)) }
func (b *byteBufferPool) Put(v []byte) { b.p.Put(&v) }

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// reverseProxy builds the streaming reverse proxy for one route. Paths,
// queries, methods and headers are passed through unchanged (apart from
// hop-by-hop headers), so Xray paths and gRPC service names stay exact.
func (s *Server) reverseProxy(rt *route) http.Handler {
	scheme := "http"
	var h1, h2 http.RoundTripper
	switch rt.Kind {
	case config.KindAuto:
		h1, h2 = s.h1Transport(), s.h2cTransport()
	case config.KindH1:
		h1 = s.h1Transport()
	case config.KindH2C:
		h2 = s.h2cTransport()
	case config.KindTLS:
		scheme = "https"
		h1 = s.tlsTransport(rt)
	}
	pick := func(r *http.Request) http.RoundTripper {
		if h1 == nil || (h2 != nil && isGRPC(r) && r.Header.Get("Upgrade") == "") {
			return h2
		}
		return h1
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = scheme
			pr.Out.URL.Host = rt.Addr
			if rt.StripPrefix {
				pr.Out.URL.Path = ensureSlash(strings.TrimPrefix(pr.In.URL.Path, rt.Prefix))
				if pr.In.URL.RawPath != "" {
					pr.Out.URL.RawPath = ensureSlash(strings.TrimPrefix(pr.In.URL.RawPath, rt.Prefix))
				}
			}
			switch {
			case rt.HostFromTarget:
				pr.Out.Host = rt.Addr
			case rt.HostHeader != "":
				pr.Out.Host = rt.HostHeader
			default:
				pr.Out.Host = pr.In.Host
			}
			if rt.XForwarded {
				if v := pr.In.Header["X-Forwarded-For"]; len(v) > 0 {
					pr.Out.Header["X-Forwarded-For"] = append([]string(nil), v...)
				}
				pr.SetXForwarded()
			}
		},
		Transport:     roundTripperFunc(func(r *http.Request) (*http.Response, error) { return pick(r).RoundTrip(r) }),
		FlushInterval: -1, // write every chunk immediately: no buffering of streams
		BufferPool:    bufPool,
		ErrorLog:      nil,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.proxyError(w, r, rt, err)
		},
	}
}

func (s *Server) proxyError(w http.ResponseWriter, r *http.Request, rt *route, err error) {
	if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
		return // the client went away; nothing to report
	}
	s.log.Warn("backend error", "route", rt.Prefix, "backend", rt.Addr, "error", err.Error())
	if isGRPC(r) {
		// A trailers-only gRPC reply so clients see UNAVAILABLE, not a
		// protocol error.
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Grpc-Status", "14")
		w.Header().Set("Grpc-Message", "relay: backend unavailable")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write([]byte("502 bad gateway\n"))
}

func ensureSlash(p string) string {
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

func (s *Server) dialer() *net.Dialer {
	return &net.Dialer{
		Timeout: s.cfg.DialTimeout,
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     s.cfg.TCPKeepAlive,
			Interval: s.cfg.TCPKeepAlive / 2,
			Count:    4,
		},
	}
}

func (s *Server) baseTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil, // never use the environment's proxy settings
		DialContext:           s.dialer().DialContext,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       s.cfg.BackendIdleTimeout,
		DisableCompression:    true, // pass bodies through untouched
		ExpectContinueTimeout: time.Second,
		WriteBufferSize:       32 << 10,
		ReadBufferSize:        32 << 10,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: 30 * time.Second,
			PingTimeout:     15 * time.Second,
		},
	}
}

func (s *Server) h1Transport() *http.Transport {
	t := s.baseTransport()
	var p http.Protocols
	p.SetHTTP1(true)
	t.Protocols = &p
	return t
}

func (s *Server) h2cTransport() *http.Transport {
	t := s.baseTransport()
	var p http.Protocols
	p.SetUnencryptedHTTP2(true) // HTTP/2 with prior knowledge, no TLS
	t.Protocols = &p
	return t
}

func (s *Server) tlsTransport(rt *route) *http.Transport {
	t := s.baseTransport()
	host, _, _ := net.SplitHostPort(rt.Addr)
	t.TLSClientConfig = &tls.Config{
		ServerName:         firstNonEmpty(rt.SNI, host),
		InsecureSkipVerify: rt.Insecure, //nolint:gosec // operator opt-in per route
		MinVersion:         tls.VersionTLS12,
	}
	t.ForceAttemptHTTP2 = true
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetHTTP2(true)
	t.Protocols = &p
	return t
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
