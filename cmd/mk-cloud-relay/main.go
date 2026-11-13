// Command mk-cloud-relay is a protocol-agnostic HTTP relay for Google Cloud
// Run and similar platforms.
//
//	mk-cloud-relay                 run the relay (configured by environment)
//	mk-cloud-relay check-config    validate the environment and print routes
//	mk-cloud-relay client ...      local TCP listener for tunnel:// routes
//	mk-cloud-relay version
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Chechimk/mk-cloud-relay/internal/client"
	"github.com/Chechimk/mk-cloud-relay/internal/config"
	"github.com/Chechimk/mk-cloud-relay/internal/relay"
)

var version = "dev"

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "client":
		os.Exit(client.Main(os.Args[2:]))
	case "version", "-version", "--version":
		fmt.Println("mk-cloud-relay", version)
		return
	case "check-config":
		cfg, err := config.FromEnv(os.Getenv)
		if err != nil {
			fmt.Fprintln(os.Stderr, "config error:", err)
			os.Exit(2)
		}
		for _, r := range cfg.Routes {
			fmt.Printf("%-24s -> %s %s\n", r.Prefix, r.Kind, r.Addr)
		}
		if cfg.ForwardUpstream != nil {
			fmt.Printf("%-24s -> upstream %s (%d users)\n", "forward proxy", cfg.ForwardUpstream.Host, len(cfg.ForwardUsers))
		}
		return
	case "", "serve":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (use serve, client, check-config or version)\n", cmd)
		os.Exit(2)
	}

	cfg, err := config.FromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}
	log := newLogger(cfg.LogLevel)
	srv := relay.New(cfg, log).HTTPServer(":" + cfg.Port)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Cloud Run gives about 10 seconds after SIGTERM.
		sctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("mk-cloud-relay started", "version", version, "port", cfg.Port, "routes", len(cfg.Routes),
		"forward_proxy", cfg.ForwardUpstream != nil)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server stopped", "error", err.Error())
		os.Exit(1)
	}
}

// newLogger writes JSON lines that Cloud Logging understands (severity and
// message fields).
func newLogger(level string) *slog.Logger {
	var lv slog.Level
	_ = lv.UnmarshalText([]byte(level))
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: lv,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.LevelKey:
				a.Key = "severity"
				if l, ok := a.Value.Any().(slog.Level); ok && l == slog.LevelWarn {
					a.Value = slog.StringValue("WARNING")
				}
			case slog.MessageKey:
				a.Key = "message"
			}
			return a
		},
	})
	return slog.New(h)
}
