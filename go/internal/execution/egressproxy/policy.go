package egressproxy

import (
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/internal/pyval"
)

// Which hosts the Docker sandbox may reach, sorted by what they let code in the sandbox DO
// (python: lha.execution.egress_hosts).
//
// The command classifier gates git push, npm publish, curl and friends, but it deliberately does
// not look inside interpreter one-liners (python -c, node -e): with no network that is harmless.
// Once the sandbox has an egress allow-list, such a one-liner can send anything to any allowed
// host, and the proxy cannot tell a download from an upload (CONNECT is end-to-end TLS). So the
// allow-list is split by what a host accepts, and a host that accepts writes has to be
// acknowledged by name:
//
//   - LHA_SANDBOX_EGRESS: package registries' download hosts; every entry must be one of
//     PackageFetchHosts (exact names, default ports).
//   - LHA_SANDBOX_EGRESS_EXTRA_HOSTS: any other host; an entry that covers a known code-hosting /
//     upload / object-store host (WriteHosts) is refused.
//   - LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS: hosts the operator accepts code in the sandbox may
//     push or upload to.
//
// This is a speed bump for the obvious channels, not a guarantee that a "fetch" host is
// read-only (npm and crates.io accept publish with a token, storage.googleapis.com accepts writes
// a signed URL grants, a request path can carry data). Any sandbox egress therefore counts as
// untrusted input + external comms under the Rule of Two. spec/execution/sandbox_egress.json pins
// this against the Python implementation.

// PackageFetchHosts are the download hosts of the common package registries (Python, Node, Go,
// Rust). storage.googleapis.com is here because proxy.golang.org redirects module zips there.
var PackageFetchHosts = []string{
	"pypi.org",
	"files.pythonhosted.org",
	"registry.npmjs.org",
	"proxy.golang.org",
	"sum.golang.org",
	"storage.googleapis.com",
	"crates.io",
	"static.crates.io",
	"index.crates.io",
}

// WriteHosts are known hosts that accept pushes or uploads: code hosting, package upload
// endpoints, object stores, container registries, paste and webhook services. A leading dot
// covers the domain and its subdomains, as in the allow-list itself.
var WriteHosts = []string{
	".github.com",
	".gitlab.com",
	".bitbucket.org",
	".codeberg.org",
	".sr.ht",
	".dev.azure.com",
	".visualstudio.com",
	".huggingface.co",
	"upload.pypi.org",
	".test.pypi.org",
	".amazonaws.com",
	".blob.core.windows.net",
	".r2.cloudflarestorage.com",
	".digitaloceanspaces.com",
	".backblazeb2.com",
	".docker.io",
	"ghcr.io",
	".pkg.dev",
	".pastebin.com",
	"transfer.sh",
	"hooks.slack.com",
	".discord.com",
	".webhook.site",
}

// The settings SandboxAllowList's errors name.
const (
	EgressSetting = "LHA_SANDBOX_EGRESS"
	ExtraSetting  = "LHA_SANDBOX_EGRESS_EXTRA_HOSTS"
	WriteSetting  = "LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS"
)

func parseSetting(setting string, items []string) ([]AllowEntry, error) {
	entries, err := ParseAllowList(items)
	if err != nil {
		return nil, pyval.NewError("ValueError", setting+": "+err.Error())
	}
	return entries, nil
}

// overlaps reports whether entry lets the sandbox reach any host known names.
func overlaps(entry, known AllowEntry) bool {
	return known.MatchesHost(entry.Host) || (entry.Suffix && entry.MatchesHost(known.Host))
}

// SandboxAllowList is the proxy allow-list for the three settings, or a ValueError-typed error
// naming the setting to use: egress entries must be package-fetch hosts, extra entries must not
// cover a known write host, write entries are accepted as acknowledged. It returns the canonical
// entries, de-duplicated, in setting order.
func SandboxAllowList(egress, extra, write []string) ([]string, error) {
	fetch := map[string]bool{}
	for _, h := range PackageFetchHosts {
		fetch[h] = true
	}
	knownWrites, err := ParseAllowList(WriteHosts)
	if err != nil {
		return nil, err
	}
	out := []string{}
	seen := map[string]bool{}
	add := func(e AllowEntry) {
		if s := e.String(); !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	entries, err := parseSetting(EgressSetting, egress)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Suffix || e.Port != 0 || !fetch[e.Host] {
			return nil, pyval.NewError("ValueError", EgressSetting+" entry "+e.String()+
				" is not a package-fetch host ("+strings.Join(PackageFetchHosts, ", ")+
				"); list any other host in "+ExtraSetting+", or one that accepts pushes or uploads in "+
				WriteSetting)
		}
		add(e)
	}
	if entries, err = parseSetting(ExtraSetting, extra); err != nil {
		return nil, err
	}
	for _, e := range entries {
		for _, known := range knownWrites {
			if overlaps(e, known) {
				return nil, pyval.NewError("ValueError", ExtraSetting+" entry "+e.String()+
					" reaches "+known.String()+", which accepts pushes or uploads: code in the "+
					"sandbox could send the workspace there. List it in "+WriteSetting+" to accept that")
			}
		}
		add(e)
	}
	if entries, err = parseSetting(WriteSetting, write); err != nil {
		return nil, err
	}
	for _, e := range entries {
		add(e)
	}
	return out, nil
}
