package spec

import (
	"reflect"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/egressproxy"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/tools"
)

func TestExecutionSandboxEgress(t *testing.T) {
	var s struct {
		PackageFetchHosts []string `json:"package_fetch_hosts"`
		WriteHosts        []string `json:"write_hosts"`
		AllowList         []struct {
			Egress []string `json:"egress"`
			Extra  []string `json:"extra"`
			Write  []string `json:"write"`
			Hosts  []string `json:"hosts"`
			Error  *string  `json:"error"`
		} `json:"allow_list"`
		RuleOfTwo []struct {
			Env           map[string]string `json:"env"`
			EgressEnabled bool              `json:"egress_enabled"`
			Error         *string           `json:"error"`
		} `json:"rule_of_two"`
		ProxyLog struct {
			Lines  []string `json:"lines"`
			Events []struct {
				Decision string `json:"decision"`
				Method   string `json:"method"`
				Host     string `json:"host"`
				Port     int    `json:"port"`
				Detail   string `json:"detail"`
				Count    int    `json:"count"`
			} `json:"events"`
		} `json:"proxy_log"`
	}
	Load(t, "execution/sandbox_egress.json", &s)
	if !reflect.DeepEqual(s.PackageFetchHosts, egressproxy.PackageFetchHosts) ||
		!reflect.DeepEqual(s.WriteHosts, egressproxy.WriteHosts) {
		t.Fatalf("host lists differ: %v / %v", egressproxy.PackageFetchHosts, egressproxy.WriteHosts)
	}
	if len(s.AllowList) == 0 || len(s.RuleOfTwo) == 0 || len(s.ProxyLog.Events) == 0 {
		t.Fatal("missing sandbox egress sections")
	}
	for _, c := range s.AllowList {
		got, err := egressproxy.SandboxAllowList(c.Egress, c.Extra, c.Write)
		switch {
		case c.Error != nil && (err == nil || err.Error() != *c.Error):
			t.Errorf("SandboxAllowList(%v, %v, %v) error = %v, want %q", c.Egress, c.Extra, c.Write, err, *c.Error)
		case c.Error == nil && (err != nil || !reflect.DeepEqual(got, c.Hosts)):
			t.Errorf("SandboxAllowList(%v, %v, %v) = %v, %v; want %v", c.Egress, c.Extra, c.Write, got, err, c.Hosts)
		}
	}
	for _, c := range s.RuleOfTwo {
		env := []string{}
		for k, v := range c.Env {
			env = append(env, "LHA_"+strings.ToUpper(k)+"="+v)
		}
		settings, err := config.LoadFrom(env, "")
		if err != nil {
			t.Fatal(err)
		}
		if settings.SandboxEgressEnabled() != c.EgressEnabled {
			t.Errorf("%v: SandboxEgressEnabled = %v", c.Env, !c.EgressEnabled)
		}
		for name, check := range map[string]func(*config.Settings) error{
			"tools": tools.CheckRunRuleOfTwo, "agent": agent.CheckRunRuleOfTwo,
		} {
			err := check(settings)
			switch {
			case c.Error == nil && err != nil:
				t.Errorf("%s %v: unexpected %v", name, c.Env, err)
			case c.Error != nil && (err == nil || err.Error() != *c.Error):
				t.Errorf("%s %v:\n got %v\nwant %s", name, c.Env, err, *c.Error)
			}
		}
	}
	events := egressproxy.ParseProxyLog(s.ProxyLog.Lines)
	if len(events) != len(s.ProxyLog.Events) {
		t.Fatalf("events = %v", events)
	}
	for i, want := range s.ProxyLog.Events {
		got := events[i].Payload
		if events[i].Kind != "sandbox_egress" || got["decision"] != want.Decision || got["method"] != want.Method ||
			got["host"] != want.Host || got["port"] != want.Port || got["detail"] != want.Detail ||
			got["count"] != want.Count {
			t.Errorf("event %d = %v, want %+v", i, got, want)
		}
	}
}
