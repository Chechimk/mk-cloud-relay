// Package client implements "mk-cloud-relay client": a local TCP listener
// whose connections are carried to a relay tunnel:// route over WebSocket.
//
// Example: forward local port 8118 to an HTTP proxy behind the relay, then
// use 127.0.0.1:8118 as a normal HTTP/HTTPS proxy in any application.
package client

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/Chechimk/mk-cloud-relay/internal/relay"
)

// Options configures a client.
type Options struct {
	Listen   string
	URL      string
	Token    string
	Insecure bool
	Ping     time.Duration
}

// Main runs the client command and returns a process exit code.
func Main(args []string) int {
	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	o := Options{}
	fs.StringVar(&o.Listen, "listen", "127.0.0.1:8118", "local address to accept connections on")
	fs.StringVar(&o.URL, "url", "", "relay tunnel URL, e.g. wss://relay.example.run.app/t/proxy")
	fs.StringVar(&o.Token, "token", "", "tunnel token (or set MK_RELAY_TOKEN)")
	fs.BoolVar(&o.Insecure, "insecure", false, "skip TLS certificate checks (testing only)")
	fs.DurationVar(&o.Ping, "ping", 25*time.Second, "WebSocket keepalive interval (0 = off)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if o.Token == "" {
		o.Token = os.Getenv("MK_RELAY_TOKEN")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := Run(ctx, o, log); err != nil {
		log.Error(err.Error())
		return 1
	}
	return 0
}

// Run listens on o.Listen until ctx is cancelled.
func Run(ctx context.Context, o Options, log *slog.Logger) error {
	ln, err := Listen(o)
	if err != nil {
		return err
	}
	log.Info("listening", "local", ln.Addr().String(), "relay", redact(o.URL))
	go func() { <-ctx.Done(); ln.Close() }()
	return Serve(ctx, ln, o, log)
}

// Listen validates the options and opens the local listener.
func Listen(o Options) (net.Listener, error) {
	u, err := url.Parse(o.URL)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return nil, errors.New("-url must be ws:// or wss://host/path")
	}
	if o.Token == "" {
		return nil, errors.New("a tunnel token is required (-token or MK_RELAY_TOKEN)")
	}
	return net.Listen("tcp", o.Listen)
}

// Serve accepts connections from ln and tunnels each one.
func Serve(ctx context.Context, ln net.Listener, o Options, log *slog.Logger) error {
	hc := &http.Client{Transport: &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: o.Insecure}, //nolint:gosec // explicit flag
		ForceAttemptHTTP2: false,
	}}
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go func() {
			if err := handle(ctx, c, hc, o); err != nil {
				log.Warn("tunnel failed", "error", err.Error())
			}
		}()
	}
}

func handle(ctx context.Context, local net.Conn, hc *http.Client, o Options) error {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ws, resp, err := websocket.Dial(dctx, o.URL, &websocket.DialOptions{
		HTTPClient:      hc,
		HTTPHeader:      http.Header{"Authorization": {"Bearer " + o.Token}},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		local.Close()
		if resp != nil {
			return fmt.Errorf("relay answered %s", resp.Status)
		}
		return err
	}
	relay.ServeWebSocketPipe(ctx, ws, local, o.Ping)
	return nil
}

func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "?"
	}
	u.RawQuery = ""
	return u.String()
}
