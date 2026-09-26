package main

import (
	"context"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs/tracing"
)

// tracingComponent is the lha.component resource attribute for a command line (python's CLI
// callback: "worker" for `lha worker`, else "cli").
func tracingComponent(args []string) string {
	if len(args) > 0 && args[0] == "worker" {
		return "worker"
	}
	return "cli"
}

// startTracing is the process-start trace exporter setup (python: the CLI callback's
// configure_tracing). It returns the matching shutdown, which flushes pending spans for at most
// the export timeout. Settings that fail to load leave tracing off (the command reports them).
func startTracing(args []string) (shutdown func()) {
	settings, err := config.Load()
	if err != nil {
		return func() {}
	}
	if tracing.Configure(settings, tracing.Options{Component: tracingComponent(args)}) == nil {
		return func() {}
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(settings.OTelExportTimeoutS)*time.Second)
		defer cancel()
		tracing.Shutdown(ctx)
	}
}
