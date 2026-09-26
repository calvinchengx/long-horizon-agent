package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/calvinchengx/long-horizon-agent/go/internal/state/vendor"
)

// vendorOptions are the fetch seams (tests replace the network).
var vendorOptions = vendor.Options{}

const vendorHelp = "Snapshot reference material into the workspace, with a SHA-256 manifest."

// vendorCmd is `lha vendor URL... [--into DIR]` (python: the vendor command): each page is
// fetched through the egress rules (DNS-pinned), stored under DIR/<host>/<path>, and listed in
// DIR/MANIFEST.json. A refused or failed URL exits 2 with "error: <reason>".
func (c *cli) vendorCmd(args []string) error {
	fs := c.newFlags("vendor", vendorHelp)
	fs.Usage = func() {
		fmt.Fprintf(c.stderr, "Usage: lha vendor [OPTIONS] URL...\n\n%s\n\n"+
			"Pass the directory to missions with --reference so the agent reads it offline.\n\nOptions:\n", vendorHelp)
		fs.PrintDefaults()
	}
	into := fs.String("into", "reference", "Directory (inside the mission workspace) to store them in.")
	urls, err := parseInterspersed(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return &exitError{code: 2}
	}
	if len(urls) == 0 {
		return &exitError{code: 2, message: "Usage: lha vendor [OPTIONS] URL...\nTry 'lha vendor --help' for help.\n\n" +
			"Error: Missing argument 'URL...'."}
	}
	saved, err := vendor.VendorURLs(c.ctx, urls, *into, vendorOptions)
	var verr *vendor.Error
	if errors.As(err, &verr) || vendor.IsEgressDenied(err) {
		return fail(2, "%s", err)
	}
	if err != nil {
		return err
	}
	for _, item := range saved {
		fmt.Fprintf(c.stdout, "%s -> %s/%s (%d bytes, sha256 %s)\n", item.URL, *into, item.Path, item.Bytes, item.SHA256[:12])
	}
	fmt.Fprintf(c.stdout, "manifest: %s/%s\n", *into, vendor.Manifest)
	return nil
}

// parseInterspersed parses options anywhere among the positional arguments (click's rule; the
// flag package stops at the first positional). Everything after "--" is positional.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		consumed := len(rest) - fs.NArg()
		stoppedAtDashes := consumed > 0 && rest[consumed-1] == "--"
		rest = fs.Args()
		if stoppedAtDashes || len(rest) == 0 {
			return append(positional, rest...), nil
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
}
