package egressproxy

import (
	"reflect"
	"testing"
)

// The cases of spec/execution/sandbox_egress.json, repeated here so the package's own tests pin them (the mutation
// audit runs only these; internal/spec runs the JSON itself).

func TestSpecSandboxAllowList(t *testing.T) {
	if want := []string{"pypi.org", "files.pythonhosted.org", "registry.npmjs.org", "proxy.golang.org", "sum.golang.org", "storage.googleapis.com", "crates.io", "static.crates.io", "index.crates.io"}; !reflect.DeepEqual(PackageFetchHosts, want) {
		t.Errorf("PackageFetchHosts = %v, want %v", PackageFetchHosts, want)
	}
	if want := []string{".github.com", ".gitlab.com", ".bitbucket.org", ".codeberg.org", ".sr.ht", ".dev.azure.com", ".visualstudio.com", ".huggingface.co", "upload.pypi.org", ".test.pypi.org", ".amazonaws.com", ".blob.core.windows.net", ".r2.cloudflarestorage.com", ".digitaloceanspaces.com", ".backblazeb2.com", ".docker.io", "ghcr.io", ".pkg.dev", ".pastebin.com", "transfer.sh", "hooks.slack.com", ".discord.com", ".webhook.site"}; !reflect.DeepEqual(WriteHosts, want) {
		t.Errorf("WriteHosts = %v, want %v", WriteHosts, want)
	}
	for _, c := range []struct {
		egress, extra, write, hosts []string
		err                         string
	}{
		{[]string{}, []string{}, []string{}, []string{}, ""},
		{[]string{"proxy.golang.org", "sum.golang.org", "storage.googleapis.com"}, []string{}, []string{}, []string{"proxy.golang.org", "sum.golang.org", "storage.googleapis.com"}, ""},
		{[]string{"PyPI.org.", "files.pythonhosted.org", "pypi.org"}, []string{}, []string{}, []string{"pypi.org", "files.pythonhosted.org"}, ""},
		{[]string{"github.com"}, []string{}, []string{}, []string{}, "LHA_SANDBOX_EGRESS entry github.com is not a package-fetch host (pypi.org, files.pythonhosted.org, registry.npmjs.org, proxy.golang.org, sum.golang.org, storage.googleapis.com, crates.io, static.crates.io, index.crates.io); list any other host in LHA_SANDBOX_EGRESS_EXTRA_HOSTS, or one that accepts pushes or uploads in LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS"},
		{[]string{".golang.org"}, []string{}, []string{}, []string{}, "LHA_SANDBOX_EGRESS entry .golang.org is not a package-fetch host (pypi.org, files.pythonhosted.org, registry.npmjs.org, proxy.golang.org, sum.golang.org, storage.googleapis.com, crates.io, static.crates.io, index.crates.io); list any other host in LHA_SANDBOX_EGRESS_EXTRA_HOSTS, or one that accepts pushes or uploads in LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS"},
		{[]string{"pypi.org:8443"}, []string{}, []string{}, []string{}, "LHA_SANDBOX_EGRESS entry pypi.org:8443 is not a package-fetch host (pypi.org, files.pythonhosted.org, registry.npmjs.org, proxy.golang.org, sum.golang.org, storage.googleapis.com, crates.io, static.crates.io, index.crates.io); list any other host in LHA_SANDBOX_EGRESS_EXTRA_HOSTS, or one that accepts pushes or uploads in LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS"},
		{[]string{"10.0.0.1"}, []string{}, []string{}, []string{}, "LHA_SANDBOX_EGRESS: invalid egress allow-list entry '10.0.0.1' (expected a DNS name)"},
		{[]string{}, []string{"mirror.internal.example", ".docs.example"}, []string{}, []string{"mirror.internal.example", ".docs.example"}, ""},
		{[]string{}, []string{"api.github.com"}, []string{}, []string{}, "LHA_SANDBOX_EGRESS_EXTRA_HOSTS entry api.github.com reaches .github.com, which accepts pushes or uploads: code in the sandbox could send the workspace there. List it in LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS to accept that"},
		{[]string{}, []string{".com"}, []string{}, []string{}, "LHA_SANDBOX_EGRESS_EXTRA_HOSTS entry .com reaches .github.com, which accepts pushes or uploads: code in the sandbox could send the workspace there. List it in LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS to accept that"},
		{[]string{}, []string{"bucket.s3.eu-west-1.amazonaws.com"}, []string{}, []string{}, "LHA_SANDBOX_EGRESS_EXTRA_HOSTS entry bucket.s3.eu-west-1.amazonaws.com reaches .amazonaws.com, which accepts pushes or uploads: code in the sandbox could send the workspace there. List it in LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS to accept that"},
		{[]string{}, []string{".pypi.org"}, []string{}, []string{}, "LHA_SANDBOX_EGRESS_EXTRA_HOSTS entry .pypi.org reaches upload.pypi.org, which accepts pushes or uploads: code in the sandbox could send the workspace there. List it in LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS to accept that"},
		{[]string{}, []string{"http://x.org"}, []string{}, []string{}, "LHA_SANDBOX_EGRESS_EXTRA_HOSTS: invalid port in egress allow-list entry 'http://x.org'"},
		{[]string{}, []string{}, []string{"github.com", ".amazonaws.com", "upload.pypi.org"}, []string{"github.com", ".amazonaws.com", "upload.pypi.org"}, ""},
		{[]string{}, []string{}, []string{"x.org:99999"}, []string{}, "LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS: invalid port in egress allow-list entry 'x.org:99999'"},
		{[]string{"pypi.org"}, []string{"pypi.org", "mirror.example"}, []string{"mirror.example", "github.com"}, []string{"pypi.org", "mirror.example", "github.com"}, ""},
	} {
		got, err := SandboxAllowList(c.egress, c.extra, c.write)
		switch {
		case c.err != "" && (err == nil || err.Error() != c.err):
			t.Errorf("SandboxAllowList(%v, %v, %v) error = %v, want %q", c.egress, c.extra, c.write, err, c.err)
		case c.err == "" && (err != nil || !reflect.DeepEqual(got, c.hosts)):
			t.Errorf("SandboxAllowList(%v, %v, %v) = %v, %v; want %v", c.egress, c.extra, c.write, got, err, c.hosts)
		}
	}
}
