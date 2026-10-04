package main

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"os/signal"
	"syscall"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/serve"
)

// mcp is lha mcp: spec/serve/mcp.json's tools on stdin and stdout (python: lha mcp).
func (c *cli) mcp(args []string) error {
	fs := c.newFlags("mcp", commandHelpFor("mcp")+
		"\n\nThe tools read missions and steer or snooze one; none answers a gate, aborts or edits the"+
		"\nchecklist. Reads the mission store (`lha config`), each mission's anchor, and Temporal for"+
		"\ndurable missions. `lha serve` answers the same tools at /mcp.")
	if err := c.parse(fs, args); err != nil {
		return err
	}
	settings, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(c.ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := persistence.OpenStore(ctx, settings, "")
	if err != nil {
		return err
	}
	defer store.Close()
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	srv := &serve.Server{Settings: settings, Store: store, Token: base64.RawURLEncoding.EncodeToString(raw), Version: Version}
	return srv.ServeMCPStdio(ctx, os.Stdin, c.stdout)
}
