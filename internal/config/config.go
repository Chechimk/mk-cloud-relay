// Package config reads MK Cloud Relay settings from environment variables.
//
// Every backend address comes from the operator's configuration. Clients can
// never choose where the relay connects, which keeps the relay from being used
// as an open proxy or for server-side request forgery.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kind says how a route talks to its backend.
type Kind string

const (
	// KindAuto (http://) sends gRPC requests as HTTP/2 cleartext (h2c) and
	// everything else, including WebSocket and HTTPUpgrade, as HTTP/1.1.
	KindAuto Kind = "auto"
	// KindH1 always uses HTTP/1.1 to the backend.
	KindH1 Kind = "h1"
	// KindH2C always uses HTTP/2 cleartext with prior knowledge.
	KindH2C Kind = "h2c"
	// KindTLS (https://) uses TLS to the backend and negotiates h2 or HTTP/1.1.
	KindTLS Kind = "https"
	// KindTunnel (tunnel://) accepts an authenticated WebSocket and pipes its
	// bytes to a fixed TCP address.
	KindTunnel Kind = "tunnel"
)

// Route maps a URL path prefix to one backend.
type Route struct {
	Prefix         string
	Kind           Kind
	Addr           string // host:port of the backend
	HostHeader     string // literal Host header for the backend ("" keeps the client's)
	HostFromTarget bool   // send the backend address as the Host header
	SNI            string
	Insecure       bool // skip TLS certificate checks (https:// only)
	StripPrefix    bool
	XForwarded     bool
	Tokens         []string // tunnel tokens for this route (falls back to TUNNEL_TOKENS)
}

// Config is the complete relay configuration.
type Config struct {
	Port               string
	Routes             []Route
	TunnelTokens       []string
	MaxTunnels         int
	ForwardUpstream    *url.URL
	ForwardUsers       map[string]string
	DialTimeout        time.Duration
	ReadHeaderTimeout  time.Duration
	IdleTimeout        time.Duration
	BackendIdleTimeout time.Duration
	TCPKeepAlive       time.Duration
	WSPingInterval     time.Duration
	MaxConcurrentH2    uint32
	LogLevel           string
	LogRequests        bool
	HealthPath         string
}

// FromEnv builds a Config from environment lookups (os.Getenv in production).
func FromEnv(get func(string) string) (*Config, error) {
	c := &Config{
		Port:         firstNonEmpty(get("PORT"), "8080"),
		LogLevel:     strings.ToLower(firstNonEmpty(get("LOG_LEVEL"), "info")),
		HealthPath:   get("HEALTH_PATH"),
		ForwardUsers: map[string]string{},
	}
	var err error
	durations := []struct {
		dst *time.Duration
		key string
		def time.Duration
	}{
		{&c.DialTimeout, "DIAL_TIMEOUT", 10 * time.Second},
		{&c.ReadHeaderTimeout, "READ_HEADER_TIMEOUT", 20 * time.Second},
		{&c.IdleTimeout, "IDLE_TIMEOUT", 120 * time.Second},
		{&c.BackendIdleTimeout, "BACKEND_IDLE_TIMEOUT", 90 * time.Second},
		{&c.TCPKeepAlive, "TCP_KEEPALIVE", 30 * time.Second},
		{&c.WSPingInterval, "WS_PING_INTERVAL", 25 * time.Second},
	}
	for _, d := range durations {
		if *d.dst, err = duration(get(d.key), d.def); err != nil {
			return nil, fmt.Errorf("%s: %w", d.key, err)
		}
	}
	if c.MaxTunnels, err = intValue(get("MAX_TUNNELS"), 1000); err != nil {
		return nil, fmt.Errorf("MAX_TUNNELS: %w", err)
	}
	h2, err := intValue(get("MAX_CONCURRENT_STREAMS"), 1000)
	if err != nil || h2 < 1 {
		return nil, errors.New("MAX_CONCURRENT_STREAMS: must be a positive number")
	}
	c.MaxConcurrentH2 = uint32(h2)
	if c.LogRequests, err = boolValue(get("LOG_REQUESTS"), false); err != nil {
		return nil, fmt.Errorf("LOG_REQUESTS: %w", err)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return nil, fmt.Errorf("LOG_LEVEL: unknown level %q", c.LogLevel)
	}
	if p, err := strconv.Atoi(c.Port); err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("PORT: invalid port %q", c.Port)
	}
	if c.HealthPath != "" && !strings.HasPrefix(c.HealthPath, "/") {
		return nil, errors.New("HEALTH_PATH: must start with /")
	}

	if c.Routes, err = ParseRoutes(get("ROUTES")); err != nil {
		return nil, fmt.Errorf("ROUTES: %w", err)
	}
	// A single default backend, also accepted under the names used by other
	// relays so existing deployments can switch without renaming variables.
	for _, key := range []string{"DEFAULT_TARGET", "PROXY_TARGET", "PROXY_TARGETS"} {
		v := strings.TrimSpace(get(key))
		if v == "" {
			continue
		}
		if strings.ContainsAny(v, ",;\n") {
			return nil, fmt.Errorf("%s: only one backend is allowed here; use ROUTES for several", key)
		}
		r, err := parseRoute("/=" + v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		c.Routes = append(c.Routes, r)
		break
	}
	if err := checkDuplicates(c.Routes); err != nil {
		return nil, err
	}
	sort.SliceStable(c.Routes, func(i, j int) bool { return len(c.Routes[i].Prefix) > len(c.Routes[j].Prefix) })

	c.TunnelTokens = splitList(get("TUNNEL_TOKENS"))
	for _, t := range c.TunnelTokens {
		if len(t) < 16 {
			return nil, errors.New("TUNNEL_TOKENS: every token must be at least 16 characters")
		}
	}
	for _, r := range c.Routes {
		if r.Kind == KindTunnel && len(r.Tokens) == 0 && len(c.TunnelTokens) == 0 {
			return nil, fmt.Errorf("route %s: tunnel routes need TUNNEL_TOKENS (or token=) so they are never open", r.Prefix)
		}
	}

	if v := strings.TrimSpace(get("FORWARD_PROXY_UPSTREAM")); v != "" {
		u, err := url.Parse(v)
		if err != nil || u.Scheme != "http" || u.Host == "" || (u.Path != "" && u.Path != "/") {
			return nil, errors.New("FORWARD_PROXY_UPSTREAM: use http://[user:pass@]host:port")
		}
		if u.Port() == "" {
			u.Host = net.JoinHostPort(u.Hostname(), "80")
		}
		c.ForwardUpstream = u
		for _, pair := range splitList(get("FORWARD_PROXY_USERS")) {
			user, pass, ok := strings.Cut(pair, ":")
			if !ok || user == "" || len(pass) < 8 {
				return nil, errors.New("FORWARD_PROXY_USERS: use user:password pairs with passwords of 8+ characters")
			}
			c.ForwardUsers[user] = pass
		}
		if len(c.ForwardUsers) == 0 {
			return nil, errors.New("FORWARD_PROXY_USERS: required when FORWARD_PROXY_UPSTREAM is set (no open proxies)")
		}
	}
	if len(c.Routes) == 0 && c.ForwardUpstream == nil {
		return nil, errors.New("nothing to do: set ROUTES, DEFAULT_TARGET or FORWARD_PROXY_UPSTREAM")
	}
	return c, nil
}

// ParseRoutes parses "PREFIX=TARGET[,option...]" entries separated by ';' or
// new lines.
func ParseRoutes(s string) ([]Route, error) {
	var out []Route
	for _, line := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '\n' }) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r, err := parseRoute(line)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", line, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func parseRoute(entry string) (Route, error) {
	prefix, rest, ok := strings.Cut(entry, "=")
	if !ok {
		return Route{}, errors.New("expected PREFIX=TARGET")
	}
	r := Route{Prefix: strings.TrimSpace(prefix)}
	if !strings.HasPrefix(r.Prefix, "/") || strings.ContainsAny(r.Prefix, " \t?#") {
		return Route{}, errors.New("the path prefix must start with / and contain no spaces, ? or #")
	}
	parts := strings.Split(rest, ",")
	target := strings.TrimSpace(parts[0])
	if !strings.Contains(target, "://") {
		target = "http://" + target
	}
	u, err := url.Parse(target)
	if err != nil {
		return Route{}, fmt.Errorf("bad target: %w", err)
	}
	if u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return Route{}, errors.New("the target must be scheme://host:port with no user, path or query")
	}
	port := u.Port()
	switch u.Scheme {
	case "http":
		r.Kind, port = KindAuto, firstNonEmpty(port, "80")
	case "h2c":
		r.Kind, port = KindH2C, firstNonEmpty(port, "80")
	case "https":
		r.Kind, port = KindTLS, firstNonEmpty(port, "443")
	case "tunnel", "tcp":
		r.Kind = KindTunnel
		if port == "" {
			return Route{}, errors.New("tunnel targets need a port")
		}
	default:
		return Route{}, fmt.Errorf("unknown scheme %q (use http, h2c, https or tunnel)", u.Scheme)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return Route{}, fmt.Errorf("bad port %q", port)
	}
	r.Addr = net.JoinHostPort(u.Hostname(), port)

	for _, opt := range parts[1:] {
		key, val, _ := strings.Cut(strings.TrimSpace(opt), "=")
		switch strings.ToLower(key) {
		case "":
		case "host":
			if val == "backend" {
				r.HostFromTarget = true
			} else if val == "" || strings.ContainsAny(val, " /\t") {
				return Route{}, errors.New("host= needs a host name or the word backend")
			} else {
				r.HostHeader = val
			}
		case "sni":
			r.SNI = val
		case "insecure":
			r.Insecure = true
		case "strip":
			r.StripPrefix = true
		case "xff":
			r.XForwarded = true
		case "proto":
			switch strings.ToLower(val) {
			case "auto":
				r.Kind = KindAuto
			case "h1", "http1", "http/1.1":
				r.Kind = KindH1
			case "h2c":
				r.Kind = KindH2C
			default:
				return Route{}, fmt.Errorf("proto=%s: use auto, h1 or h2c", val)
			}
			if u.Scheme != "http" && u.Scheme != "h2c" {
				return Route{}, errors.New("proto= only applies to http:// and h2c:// targets")
			}
		case "token":
			for _, t := range strings.Split(val, "|") {
				if len(t) < 16 {
					return Route{}, errors.New("token= values must be at least 16 characters")
				}
				r.Tokens = append(r.Tokens, t)
			}
		default:
			return Route{}, fmt.Errorf("unknown option %q", key)
		}
	}
	if (r.SNI != "" || r.Insecure) && r.Kind != KindTLS {
		return Route{}, errors.New("sni= and insecure only apply to https:// targets")
	}
	if len(r.Tokens) > 0 && r.Kind != KindTunnel {
		return Route{}, errors.New("token= only applies to tunnel:// targets")
	}
	return r, nil
}

func checkDuplicates(rs []Route) error {
	seen := map[string]bool{}
	for _, r := range rs {
		if seen[r.Prefix] {
			return fmt.Errorf("path prefix %s is configured twice", r.Prefix)
		}
		seen[r.Prefix] = true
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' }) {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

func duration(v string, def time.Duration) (time.Duration, error) {
	if v = strings.TrimSpace(v); v == "" {
		return def, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q", v)
	}
	return d, nil
}

func intValue(v string, def int) (int, error) {
	if v = strings.TrimSpace(v); v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid number %q", v)
	}
	return n, nil
}

func boolValue(v string, def bool) (bool, error) {
	if v = strings.TrimSpace(v); v == "" {
		return def, nil
	}
	return strconv.ParseBool(v)
}
