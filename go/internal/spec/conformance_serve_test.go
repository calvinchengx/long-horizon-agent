package spec

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/serve"
)

// The UI API (spec/serve): the cases themselves run against this implementation's lha serve in
// CI (python/tests/serve, LHA_SERVE_CMD); these tests pin what the Go side owns.

func routeRegexp(template string) *regexp.Regexp {
	return regexp.MustCompile("^" + regexp.MustCompile(`\\\{[^/]+\\\}`).ReplaceAllString(regexp.QuoteMeta(template), "[^/]+") + "$")
}

// No undocumented route: the routes lha serve registers are exactly openapi.json's operations.
func TestServeRegistersExactlyTheDocumentedRoutes(t *testing.T) {
	var api map[string]any
	Load(t, "serve/openapi.json", &api)
	var documented []string
	for template, ops := range api["paths"].(map[string]any) {
		for method := range ops.(map[string]any) {
			documented = append(documented, strings.ToUpper(method)+" "+template)
		}
	}
	var registered []string
	for _, r := range serve.Routes {
		registered = append(registered, r.Method+" "+r.Path)
	}
	sort.Strings(documented)
	sort.Strings(registered)
	if strings.Join(documented, "\n") != strings.Join(registered, "\n") {
		t.Fatalf("documented:\n%s\nregistered:\n%s", strings.Join(documented, "\n"), strings.Join(registered, "\n"))
	}
	if api["info"].(map[string]any)["version"] != serve.APIVersion {
		t.Fatalf("serve.APIVersion %s is not openapi.json's %v", serve.APIVersion, api["info"])
	}
}

// Every case's request reaches a Go route (except the undocumented path, which must not).
func TestEveryServeCaseMapsOntoARoute(t *testing.T) {
	var cases struct {
		Description string           `json:"description"`
		Cases       []map[string]any `json:"cases"`
	}
	Load(t, "serve/cases.json", &cases)
	if cases.Description == "" || len(cases.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range cases.Cases {
		req := c["request"].(map[string]any)
		matched := false
		for _, r := range serve.Routes {
			if r.Method == req["method"] && routeRegexp(r.Path).MatchString(req["path"].(string)) {
				matched = true
			}
		}
		if documented := c["operation"] != ""; matched != documented {
			t.Errorf("%v: matches a route %v, documented %v", c["name"], matched, documented)
		}
	}
}

// The fixture's events are valid against the event contract, as Go judges it.
func TestServeFixtureEventsMatchTheirSchemas(t *testing.T) {
	var fixture map[string]any // numbers are json.Number, as EventErrors expects
	Load(t, "serve/fixture.json", &fixture)
	var contract eventContract
	Load(t, "obs/mission_events.json", &contract)
	events := fixture["events"].([]any)
	seen := map[string]bool{}
	for _, raw := range events {
		e := raw.(map[string]any)
		kind := e["kind"].(string)
		seen[kind] = true
		if errs := obs.EventErrors(contract.Kinds, kind, e["payload"]); len(errs) > 0 {
			t.Errorf("%s: %v", kind, errs)
		}
	}
	for kind := range contract.Kinds {
		if !seen[kind] {
			t.Errorf("the fixture records no %s event", kind)
		}
	}
	if len(fixture["missions"].([]any)) == 0 || len(fixture["anchors"].(map[string]any)) == 0 {
		t.Fatal("no missions or anchors")
	}
}
