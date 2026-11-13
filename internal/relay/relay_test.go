package relay_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Chechimk/mk-cloud-relay/internal/client"
	"github.com/Chechimk/mk-cloud-relay/internal/config"
	"github.com/Chechimk/mk-cloud-relay/internal/relay"
)

const testToken = "test-token-0123456789"

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// startRelay runs the relay on a random port and returns its address.
func startRelay(t *testing.T, env map[string]string) string {
	t.Helper()
	cfg, err := config.FromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	srv := relay.New(cfg, quiet).HTTPServer("")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

// startBackend serves h with both HTTP/1.1 and h2c.
func startBackend(t *testing.T, h http.Handler) string {
	t.Helper()
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: h, Protocols: &p}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

func h2cClient() *http.Client {
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: &p}}
}

func h1Client() *http.Client {
	var p http.Protocols
	p.SetHTTP1(true)
	return &http.Client{Transport: &http.Transport{Protocols: &p, DisableKeepAlives: false}}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

type echoInfo struct {
	Path, RawPath, Query, Host, Proto, XFF string
}

func infoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(echoInfo{
			Path: r.URL.Path, RawPath: r.URL.EscapedPath(), Query: r.URL.RawQuery,
			Host: r.Host, Proto: r.Proto, XFF: r.Header.Get("X-Forwarded-For"),
		})
	})
}

func getInfo(t *testing.T, c *http.Client, rawURL, host string) echoInfo {
	t.Helper()
	req, _ := http.NewRequest("GET", rawURL, nil)
	if host != "" {
		req.Host = host
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%s: status %d", rawURL, resp.StatusCode)
	}
	var e echoInfo
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestReverseProxyPreservesRequest(t *testing.T) {
	b := startBackend(t, infoHandler())
	r := startRelay(t, map[string]string{"ROUTES": fmt.Sprintf(
		"/keep/=http://%s; /strip/=http://%s,strip,host=backend; /xff/=http://%s,xff,host=internal.example", b, b, b)})

	e := getInfo(t, h1Client(), "http://"+r+"/keep/a%2Fb/Tun?seq=7&x=y", "front.example")
	if e.RawPath != "/keep/a%2Fb/Tun" || e.Query != "seq=7&x=y" || e.Host != "front.example" || e.Proto != "HTTP/1.1" {
		t.Errorf("path/host not preserved: %+v", e)
	}
	if e.XFF != "" {
		t.Errorf("X-Forwarded-For should be dropped by default, got %q", e.XFF)
	}
	e = getInfo(t, h1Client(), "http://"+r+"/strip/inner?q=1", "")
	if e.Path != "/inner" || e.Host != b {
		t.Errorf("strip/host=backend: %+v", e)
	}
	e = getInfo(t, h1Client(), "http://"+r+"/xff/", "")
	if e.Host != "internal.example" || !strings.HasPrefix(e.XFF, "203.0.113.9, 127.0.0.1") {
		t.Errorf("xff/host=: %+v", e)
	}
}

func TestAutoRouteProtocolSelection(t *testing.T) {
	b := startBackend(t, infoHandler())
	r := startRelay(t, map[string]string{"DEFAULT_TARGET": b})
	// Plain requests (even over HTTP/2 from the client) use HTTP/1.1 to the backend.
	if e := getInfo(t, h2cClient(), "http://"+r+"/x", ""); e.Proto != "HTTP/1.1" {
		t.Errorf("non-gRPC request went to backend as %s", e.Proto)
	}
	// gRPC uses h2c.
	req, _ := http.NewRequest("POST", "http://"+r+"/svc/Tun", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/grpc")
	resp, err := h2cClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var e echoInfo
	_ = json.NewDecoder(resp.Body).Decode(&e)
	resp.Body.Close()
	if e.Proto != "HTTP/2.0" {
		t.Errorf("gRPC request went to backend as %s", e.Proto)
	}
}

// grpcEcho behaves like a gRPC bidirectional stream: it sends headers at
// once, echoes each 8-byte message as it arrives and ends with trailers.
func grpcEcho(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			http.Error(w, "need h2", 505)
			return
		}
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		buf := make([]byte, 8)
		for {
			if _, err := io.ReadFull(r.Body, buf); err != nil {
				break
			}
			w.Write(buf)
			w.(http.Flusher).Flush()
		}
		w.Header().Set("Grpc-Status", "0")
		w.Header().Set("Grpc-Message", "done")
	})
}

func TestGRPCBidirectionalStreamingAndTrailers(t *testing.T) {
	for _, scheme := range []string{"http", "h2c"} {
		t.Run(scheme, func(t *testing.T) {
			b := startBackend(t, grpcEcho(t))
			r := startRelay(t, map[string]string{"ROUTES": "/svc.Tun/=" + scheme + "://" + b})
			pr, pw := io.Pipe()
			req, _ := http.NewRequest("POST", "http://"+r+"/svc.Tun/Tun", pr)
			req.Header.Set("Content-Type", "application/grpc")
			req.Header.Set("Te", "trailers")
			resp, err := h2cClient().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got := make([]byte, 8)
			for i := 0; i < 20; i++ {
				msg := []byte(fmt.Sprintf("msg-%04d", i))
				if _, err := pw.Write(msg); err != nil {
					t.Fatal(err)
				}
				// The reply must arrive before the next message is sent:
				// proves the relay streams both ways without buffering.
				done := make(chan error, 1)
				go func() { _, err := io.ReadFull(resp.Body, got); done <- err }()
				select {
				case err := <-done:
					if err != nil || !bytes.Equal(got, msg) {
						t.Fatalf("message %d: got %q err %v", i, got, err)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("message %d was buffered", i)
				}
			}
			pw.Close()
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatal(err)
			}
			if resp.Trailer.Get("Grpc-Status") != "0" || resp.Trailer.Get("Grpc-Message") != "done" {
				t.Errorf("trailers lost: %v", resp.Trailer)
			}
		})
	}
}

func TestStreamingResponseIsNotBuffered(t *testing.T) {
	release := make(chan struct{})
	b := startBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("first"))
		w.(http.Flusher).Flush()
		<-release
		w.Write([]byte("second"))
	}))
	r := startRelay(t, map[string]string{"DEFAULT_TARGET": b})
	resp, err := h1Client().Get("http://" + r + "/down")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 5)
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(resp.Body, buf); done <- err }()
	select {
	case err := <-done:
		if err != nil || string(buf) != "first" {
			t.Fatalf("got %q %v", buf, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first chunk was held back")
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "second" {
		t.Fatalf("rest = %q", rest)
	}
}

// XHTTP stream-one over HTTP/1.1 reads the response while still uploading.
func TestHTTP1FullDuplex(t *testing.T) {
	b := startBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).EnableFullDuplex()
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		buf := make([]byte, 5)
		for {
			if _, err := io.ReadFull(r.Body, buf); err != nil {
				return
			}
			w.Write(buf)
			w.(http.Flusher).Flush()
		}
	}))
	r := startRelay(t, map[string]string{"DEFAULT_TARGET": b})
	c, err := net.Dial("tcp", r)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprint(c, "POST /xh/session HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	for _, word := range []string{"hello", "world", "again"} {
		if word != "hello" {
			fmt.Fprintf(c, "5\r\n%s\r\n", word)
		}
		if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != word {
			t.Fatalf("want %q got %q (%v)", word, buf, err)
		}
	}
	fmt.Fprint(c, "0\r\n\r\n")
}

func TestWebSocketReverseProxy(t *testing.T) {
	b := startBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(-1)
		ctx := r.Context()
		for {
			typ, msg, err := c.Read(ctx)
			if err != nil {
				return
			}
			if err := c.Write(ctx, typ, msg); err != nil {
				return
			}
		}
	}))
	r := startRelay(t, map[string]string{"ROUTES": "/ws=http://" + b})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+r+"/ws?ed=2048", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(-1)
	big := randBytes(2 << 20)
	for _, msg := range [][]byte{[]byte("hi"), big, randBytes(70000)} {
		if err := c.Write(ctx, websocket.MessageBinary, msg); err != nil {
			t.Fatal(err)
		}
		_, got, err := c.Read(ctx)
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("echo mismatch (%d bytes): %v", len(msg), err)
		}
	}
	if err := c.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Errorf("close: %v", err)
	}
}

// Xray HTTPUpgrade: 101 response, then raw bytes without WebSocket framing.
func TestHTTPUpgradeRawStream(t *testing.T) {
	b := startBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "no", 400)
			return
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		io.Copy(conn, brw)
	}))
	r := startRelay(t, map[string]string{"ROUTES": "/hu=http://" + b})
	c, err := net.Dial("tcp", r)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprint(c, "GET /hu HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	payload := randBytes(300000)
	go c.Write(payload)
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(br, got); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("raw stream mismatch: %v", err)
	}
}

func tcpEcho(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

func TestTunnel(t *testing.T) {
	echo := tcpEcho(t)
	r := startRelay(t, map[string]string{"ROUTES": "/t/echo=tunnel://" + echo, "TUNNEL_TOKENS": testToken})

	// Plain requests: missing token, then token without upgrade.
	resp, _ := h1Client().Get("http://" + r + "/t/echo")
	if resp.StatusCode != 401 {
		t.Errorf("no token: status %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", "http://"+r+"/t/echo", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, _ = h1Client().Do(req)
	if resp.StatusCode != 400 {
		t.Errorf("no upgrade: status %d", resp.StatusCode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listen := func(token string) string {
		ln, err := client.Listen(client.Options{Listen: "127.0.0.1:0", URL: "ws://" + r + "/t/echo", Token: token})
		if err != nil {
			t.Fatal(err)
		}
		go client.Serve(ctx, ln, client.Options{URL: "ws://" + r + "/t/echo", Token: token, Ping: time.Second}, quiet)
		return ln.Addr().String()
	}
	good := listen(testToken)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("tcp", good)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(20 * time.Second))
			payload := randBytes(1 << 20)
			go c.Write(payload)
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, payload) {
				errs <- fmt.Errorf("tunnel echo mismatch: %v", err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	bad := listen("wrong-token-000000000")
	c, err := net.Dial("tcp", bad)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	c.Write([]byte("x"))
	if n, _ := c.Read(make([]byte, 1)); n != 0 {
		t.Error("a wrong token must not open the tunnel")
	}
	c.Close()
}

// upstreamProxy is a small authenticated HTTP proxy used as the relay's
// FORWARD_PROXY_UPSTREAM in tests.
func upstreamProxy(t *testing.T) string {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("up:uppass"))
	return startBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != want {
			http.Error(w, "upstream auth", http.StatusProxyAuthRequired)
			return
		}
		if r.Method == http.MethodConnect {
			dst, err := net.Dial("tcp", r.Host)
			if err != nil {
				http.Error(w, "dial", 502)
				return
			}
			c, brw, _ := http.NewResponseController(w).Hijack()
			fmt.Fprint(c, "HTTP/1.1 200 Connection established\r\n\r\n")
			go func() { io.Copy(dst, brw); dst.Close() }()
			io.Copy(c, dst)
			c.Close()
			return
		}
		(&httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = pr.In.URL
			pr.Out.Header.Del("Proxy-Authorization")
		}}).ServeHTTP(w, r)
	}))
}

func TestForwardProxy(t *testing.T) {
	up := upstreamProxy(t)
	tlsSite := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret-ok")) }))
	defer tlsSite.Close()
	plainSite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			w.Write([]byte("leaked credentials"))
			return
		}
		w.Write([]byte("plain-ok"))
	}))
	defer plainSite.Close()
	r := startRelay(t, map[string]string{
		"FORWARD_PROXY_UPSTREAM": "http://up:uppass@" + up,
		"FORWARD_PROXY_USERS":    "alice:password1",
	})
	viaRelay := func(user string) *http.Client {
		u, _ := url.Parse("http://" + user + "@" + r)
		return &http.Client{Transport: &http.Transport{
			Proxy:           http.ProxyURL(u),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}, Timeout: 10 * time.Second}
	}
	for _, target := range []struct{ url, want string }{{tlsSite.URL, "secret-ok"}, {plainSite.URL, "plain-ok"}} {
		resp, err := viaRelay("alice:password1").Get(target.url)
		if err != nil {
			t.Fatalf("%s: %v", target.url, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != target.want {
			t.Errorf("%s: got %q", target.url, body)
		}
	}
	resp, err := viaRelay("alice:wrongpass").Get(plainSite.URL)
	if err != nil || resp.StatusCode != 407 {
		t.Errorf("wrong password: %v %v", resp, err)
	}
	if _, err := viaRelay("alice:wrongpass").Get(tlsSite.URL); err == nil || !strings.Contains(err.Error(), "Proxy Authentication Required") {
		t.Errorf("CONNECT with wrong password: %v", err)
	}
}

func TestConnectRefusedWithoutForwardProxy(t *testing.T) {
	r := startRelay(t, map[string]string{"DEFAULT_TARGET": "127.0.0.1:9"})
	c, err := net.Dial("tcp", r)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != 405 {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
}

func TestBackendDownAndUnknownPaths(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	r := startRelay(t, map[string]string{"ROUTES": "/api/=http://" + dead, "HEALTH_PATH": "/_mk/health"})

	resp, err := h1Client().Get("http://" + r + "/api/x")
	if err != nil || resp.StatusCode != 502 {
		t.Fatalf("dead backend: %v %v", resp, err)
	}
	req, _ := http.NewRequest("POST", "http://"+r+"/api/Tun", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/grpc")
	resp, err = h2cClient().Do(req)
	if err != nil || resp.Header.Get("Grpc-Status") != "14" {
		t.Fatalf("dead gRPC backend: %v %v", resp, err)
	}
	resp, _ = h1Client().Get("http://" + r + "/other")
	if resp.StatusCode != 404 {
		t.Errorf("unknown path: %d", resp.StatusCode)
	}
	resp, _ = h1Client().Get("http://" + r + "/_mk/health")
	if resp.StatusCode != 200 {
		t.Errorf("health: %d", resp.StatusCode)
	}
}

func TestConcurrentStreams(t *testing.T) {
	payload := randBytes(256 << 10)
	b := startBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(payload) }))
	r := startRelay(t, map[string]string{"ROUTES": "/h1/=http://" + b + "; /h2/=h2c://" + b})
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := 0; i < 100; i++ {
		for _, p := range []string{"/h1/", "/h2/"} {
			wg.Add(1)
			go func(c *http.Client, p string) {
				defer wg.Done()
				resp, err := c.Get("http://" + r + p + "f")
				if err != nil {
					errs <- err
					return
				}
				got, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if !bytes.Equal(got, payload) {
					errs <- fmt.Errorf("%s: payload mismatch", p)
				}
			}(map[bool]*http.Client{true: h2cClient(), false: h1Client()}[i%2 == 0], p)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A backend that resets its stream right after the last byte (as Xray's gRPC
// and XHTTP servers sometimes do) must still give the client a clean end, so
// front ends like Cloud Run deliver everything they have buffered.
func TestBackendResetAfterDataEndsCleanly(t *testing.T) {
	payload := randBytes(3 << 20)
	b := startBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
		w.Write(payload)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // RST_STREAM on h2, abrupt close on HTTP/1.1
	}))
	r := startRelay(t, map[string]string{"ROUTES": "/h1/=http://" + b + "; /h2/=h2c://" + b})
	for _, c := range []struct {
		path   string
		client *http.Client
	}{{"/h1/x", h1Client()}, {"/h2/x", h2cClient()}} {
		resp, err := c.client.Get("http://" + r + c.path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || !bytes.Equal(got, payload) {
			t.Errorf("%s: got %d bytes, err %v (want a clean end after %d bytes)", c.path, len(got), err, len(payload))
		}
	}
}

// When the backend of an upgraded connection finishes while the client is
// still sending, a client that reads late must still receive every byte and a
// normal close (FIN), not a reset that throws away buffered data.
func TestUpgradeEndDeliversEverythingToSlowClient(t *testing.T) {
	payload := randBytes(8 << 20)
	b := startBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		go io.Copy(io.Discard, conn) // ignores what the client sends
		fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		conn.Write(payload)
		conn.(*net.TCPConn).CloseWrite() // finished sending
		time.Sleep(10 * time.Second)
	}))
	r := startRelay(t, map[string]string{"ROUTES": "/ws=http://" + b})
	c, err := net.Dial("tcp", r)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "GET /ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	stop := make(chan struct{})
	defer close(stop)
	go func() { // keep uploading so the relay always has unread client data
		junk := make([]byte, 4096)
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := c.Write(junk); err != nil && !isTimeout(err) {
				return
			}
		}
	}()
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	time.Sleep(500 * time.Millisecond) // let the backend finish while we have not read anything
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	var got bytes.Buffer
	buf := make([]byte, 32<<10)
	for {
		n, err := br.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			if err != io.EOF {
				t.Fatalf("after %d bytes: %v (want EOF)", got.Len(), err)
			}
			break
		}
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("got %d of %d bytes", got.Len(), len(payload))
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}
