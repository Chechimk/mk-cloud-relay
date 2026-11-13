package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestParseRoutes(t *testing.T) {
	rs, err := ParseRoutes(`
		/g1/=h2c://10.0.0.1:2001
		/xh/=http://10.0.0.1:2002,host=backend ; /ws=10.0.0.1:80,proto=h1,xff
		/cdn/=https://example.com,sni=front.example,insecure,strip
		# comment
		/t/proxy=tunnel://10.0.0.1:3128,token=0123456789abcdef|fedcba9876543210`)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		prefix string
		kind   Kind
		addr   string
	}{
		{"/g1/", KindH2C, "10.0.0.1:2001"},
		{"/xh/", KindAuto, "10.0.0.1:2002"},
		{"/ws", KindH1, "10.0.0.1:80"},
		{"/cdn/", KindTLS, "example.com:443"},
		{"/t/proxy", KindTunnel, "10.0.0.1:3128"},
	}
	if len(rs) != len(want) {
		t.Fatalf("got %d routes", len(rs))
	}
	for i, w := range want {
		if rs[i].Prefix != w.prefix || rs[i].Kind != w.kind || rs[i].Addr != w.addr {
			t.Errorf("route %d = %+v, want %+v", i, rs[i], w)
		}
	}
	if !rs[1].HostFromTarget || !rs[2].XForwarded || !rs[3].Insecure || !rs[3].StripPrefix || rs[3].SNI != "front.example" {
		t.Errorf("options not parsed: %+v", rs)
	}
	if len(rs[4].Tokens) != 2 {
		t.Errorf("tokens: %v", rs[4].Tokens)
	}
}

func TestParseRouteErrors(t *testing.T) {
	bad := []string{
		"noslash=http://a:1",
		"/a=ftp://a:1",
		"/a=http://user:pw@a:1",
		"/a=http://a:1/path",
		"/a=tunnel://a",
		"/a=http://a:99999",
		"/a=http://a:1,insecure",
		"/a=https://a,proto=h2c",
		"/a=http://a:1,token=0123456789abcdef",
		"/a=tunnel://a:1,token=short",
		"/a=http://a:1,bogus",
		"/a b=http://a:1",
	}
	for _, b := range bad {
		if _, err := ParseRoutes(b); err == nil {
			t.Errorf("%q: expected an error", b)
		}
	}
}

func TestFromEnv(t *testing.T) {
	c, err := FromEnv(env(map[string]string{
		"PROXY_TARGETS": "34.63.66.185:80",
		"ROUTES":        "/grpc/=h2c://b:1",
		"DIAL_TIMEOUT":  "3",
		"IDLE_TIMEOUT":  "2m",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != "8080" || c.DialTimeout != 3*time.Second || c.IdleTimeout != 2*time.Minute {
		t.Errorf("defaults: %+v", c)
	}
	if c.Routes[0].Prefix != "/grpc/" || c.Routes[1].Prefix != "/" || c.Routes[1].Addr != "34.63.66.185:80" {
		t.Errorf("routes not sorted longest first: %+v", c.Routes)
	}
}

func TestFromEnvSafety(t *testing.T) {
	cases := map[string]map[string]string{
		"nothing configured":        {},
		"tunnel without token":      {"ROUTES": "/t=tunnel://a:1"},
		"short global token":        {"ROUTES": "/t=tunnel://a:1", "TUNNEL_TOKENS": "short"},
		"open forward proxy":        {"FORWARD_PROXY_UPSTREAM": "http://p:3128"},
		"weak forward password":     {"FORWARD_PROXY_UPSTREAM": "http://p:3128", "FORWARD_PROXY_USERS": "a:b"},
		"https forward upstream":    {"FORWARD_PROXY_UPSTREAM": "https://p:3128", "FORWARD_PROXY_USERS": "a:12345678"},
		"duplicate prefix":          {"ROUTES": "/a=b:1;/a=c:1"},
		"default target plus slash": {"ROUTES": "/=b:1", "DEFAULT_TARGET": "c:1"},
		"several default targets":   {"DEFAULT_TARGET": "b:1,c:1"},
		"bad port":                  {"PORT": "x", "DEFAULT_TARGET": "b:1"},
		"bad log level":             {"LOG_LEVEL": "loud", "DEFAULT_TARGET": "b:1"},
		"bad duration":              {"DIAL_TIMEOUT": "-5s", "DEFAULT_TARGET": "b:1"},
	}
	for name, m := range cases {
		if _, err := FromEnv(env(m)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	c, err := FromEnv(env(map[string]string{
		"FORWARD_PROXY_UPSTREAM": "http://u:p@proxy",
		"FORWARD_PROXY_USERS":    "alice:password1,bob:password2",
	}))
	if err != nil || c.ForwardUpstream.Host != "proxy:80" || len(c.ForwardUsers) != 2 {
		t.Fatalf("forward proxy config: %v %+v", err, c)
	}
	if _, err := FromEnv(env(map[string]string{"ROUTES": "/t=tunnel://a:1", "TUNNEL_TOKENS": strings.Repeat("x", 16)})); err != nil {
		t.Fatal(err)
	}
}
