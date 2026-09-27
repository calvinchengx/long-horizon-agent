package execution

import (
	"context"
	"testing"
)

func TestDockerSessionDrainsItsProxyLog(t *testing.T) {
	ready := "2026-09-26 10:00:00,001 INFO lha-egress-proxy listening on 0.0.0.0:3128 allow=pypi.org\n"
	cli := &fakeDocker{proxyLog: ready}
	sb, err := NewDockerSandbox(DockerOptions{CLI: cli, EgressHosts: []string{"pypi.org"}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := sb.Open(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	session := s.(*DockerSandboxSession)
	if got := session.DrainEgressEvents(context.Background()); len(got) != 0 {
		t.Fatalf("ready line only: %v", got)
	}
	cli.proxyLog = ready +
		"2026-09-26 10:00:01,000 INFO allow CONNECT pypi.org:443 -> 151.101.0.223:443\n" +
		"2026-09-26 10:00:01,500 INFO allow CONNECT pypi.org:443 -> 151.101.64.223:443\n" +
		"2026-09-26 10:00:02,000 WARNING deny CONNECT github.com:443: host not in egress allow-list: github.com\n"
	got := session.DrainEgressEvents(context.Background())
	if len(got) != 2 || got[0].Kind != "sandbox_egress" || got[0].Payload.Plain()["host"] != "pypi.org" ||
		got[0].Payload.Plain()["count"] != 2 || got[1].Payload.Plain()["decision"] != "deny" {
		t.Fatalf("events = %+v", got)
	}
	if again := session.DrainEgressEvents(context.Background()); len(again) != 0 {
		t.Fatalf("drained twice: %v", again)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	plain, err := NewDockerSandbox(DockerOptions{CLI: &fakeDocker{}})
	if err != nil {
		t.Fatal(err)
	}
	ps, err := plain.Open(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := ps.(*DockerSandboxSession).DrainEgressEvents(context.Background()); got != nil {
		t.Fatalf("no egress, events %v", got)
	}
}
