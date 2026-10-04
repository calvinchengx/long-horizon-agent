package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/serve"
)

// serve is lha serve: the mission UI's API on loopback (python: lha serve).
func (c *cli) serve(args []string) error {
	fs := c.newFlags("serve", commandHelpFor("serve")+
		"\n\nPrints the start-up URL with its token first; LHA_SERVE_TOKEN fixes the token (otherwise it is"+
		"\nrandom). Reads the mission store (`lha config`), each mission's anchor, and Temporal for"+
		"\ndurable missions.")
	host := fs.String("host", "127.0.0.1", "Loopback address to bind (127.0.0.1 or localhost).")
	port := fs.Int("port", 8765, "Port (0 picks a free one).")
	if err := c.parse(fs, args); err != nil {
		return err
	}
	if *host != "127.0.0.1" && *host != "localhost" {
		return fail(2, "error: lha serve binds to loopback only, not %s", contracts.PyRepr(*host))
	}
	if *port < 0 || *port > 65535 {
		return fail(2, "error: --port must be 0-65535")
	}
	settings, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(c.ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		return fail(2, "error: %v", err)
	}
	store, err := persistence.OpenStore(ctx, settings, "")
	if err != nil {
		_ = ln.Close()
		return err
	}
	defer store.Close()
	token := os.Getenv("LHA_SERVE_TOKEN")
	if token == "" {
		raw := make([]byte, 24)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		token = base64.RawURLEncoding.EncodeToString(raw)
	}
	bound := ln.Addr().(*net.TCPAddr).Port
	srv := &serve.Server{Settings: settings, Store: store, Token: token, Port: bound, Version: Version}
	if err := srv.Start(ctx); err != nil {
		_ = ln.Close()
		return err
	}
	httpServer := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()
	fmt.Fprintf(c.stdout, "lha serve: http://127.0.0.1:%d/?token=%s\n", bound, token)
	if f, ok := c.stdout.(interface{ Sync() error }); ok {
		_ = f.Sync()
	}
	if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
