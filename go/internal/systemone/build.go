package systemone

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
)

// ConfigError is a System One configuration that cannot be used safely (python: ValueError).
type ConfigError struct{ Message string }

func (e *ConfigError) Error() string { return e.Message }

// Build is the configured model, metered by meter; nil when system_one_backend=off (python:
// build_system_one). A malformed or non-https remote endpoint, a remote endpoint without a key, an
// endpoint with no price, or a remote endpoint on a private_data run without
// system_one_private_data_ok is a *ConfigError.
func Build(s *config.Settings, meter *governor.CostMeter) (Model, error) {
	switch s.SystemOneBackend {
	case "", "off":
		return nil, nil
	case "stub":
		return &Stub{}, nil
	}
	endpoint := s.SystemOneEndpoint
	target, err := safety.ParseURL(endpoint)
	if err != nil {
		return nil, &ConfigError{Message: "LHA_SYSTEM_ONE_ENDPOINT is not usable: " + err.Error()}
	}
	local := IsLocalEndpoint(target)
	if !local && target.Scheme != "https" {
		return nil, &ConfigError{Message: "LHA_SYSTEM_ONE_ENDPOINT is not usable: a remote endpoint must use https"}
	}
	if s.PrivateData && !local && !s.SystemOnePrivateDataOK {
		return nil, &ConfigError{Message: "LHA_PRIVATE_DATA is set and LHA_SYSTEM_ONE_ENDPOINT is remote: it would receive " +
			"workspace text. Use a loopback endpoint (a self-hosted Kev) or set LHA_SYSTEM_ONE_PRIVATE_DATA_OK=true"}
	}
	key := ""
	if s.SystemOneAPIKey != nil {
		key = s.SystemOneAPIKey.Value()
	}
	if !local && strings.TrimSpace(key) == "" {
		return nil, &ConfigError{Message: "LHA_SYSTEM_ONE_API_KEY is required for a remote System One endpoint"}
	}
	price := s.SystemOnePriceInPerMTok
	if price == nil {
		if price, err = DefaultPriceInPerMTok(endpoint); err != nil {
			return nil, &ConfigError{Message: "LHA_SYSTEM_ONE_ENDPOINT is not usable: " + err.Error()}
		}
	}
	if price == nil && !s.AllowUnpricedModels {
		return nil, &ConfigError{Message: fmt.Sprintf("no price for the System One endpoint %s: set "+
			"LHA_SYSTEM_ONE_PRICE_IN_PER_MTOK (or LHA_ALLOW_UNPRICED_MODELS=true)", contracts.PyRepr(endpoint))}
	}
	c, err := NewClient(Options{Model: s.SystemOneModel, Endpoint: endpoint, APIKey: key, PriceInPerMTok: price,
		Timeout: time.Duration(s.SystemOneTimeoutS * float64(time.Second)), Meter: meter})
	if err != nil {
		return nil, &ConfigError{Message: "LHA_SYSTEM_ONE_ENDPOINT is not usable: " + err.Error()}
	}
	return c, nil
}

// BuildStallTriage is stall triage over m when it is configured and system_one_triage is on.
func BuildStallTriage(s *config.Settings, m Model) *StallTriage {
	if m == nil || !s.SystemOneTriage {
		return nil
	}
	return &StallTriage{Model: m, Threshold: s.SystemOneTriageThreshold, MinFailures: s.SystemOneTriageMinFailures}
}

// Close closes m when it holds resources.
func Close(m Model) error {
	if c, ok := m.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// IsConfigError reports a *ConfigError.
func IsConfigError(err error) bool {
	var ce *ConfigError
	return errors.As(err, &ce)
}
