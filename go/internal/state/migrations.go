package state

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Migrator upgrades a document from one schema version to the next.
//
// Over a multi-week run the progress/checklist/memory formats may evolve. Each artifact has a
// schema_version; registered migrators upgrade a document one version at a time, so old durable
// data is never silently incompatible after a deploy.
type Migrator func(data map[string]any) map[string]any

type migratorKey struct {
	artifact string
	from     int
}

var (
	migratorsMu sync.RWMutex
	migrators   = map[migratorKey]Migrator{}
)

// Register registers fn as the migrator that upgrades artifact from fromVersion to the next.
func Register(artifact string, fromVersion int, fn Migrator) {
	migratorsMu.Lock()
	defer migratorsMu.Unlock()
	migrators[migratorKey{artifact, fromVersion}] = fn
}

// asInt coerces a JSON-ish value to an int, falling back to def (Python's _as_int: bools are
// rejected, 0 means "use the default", digit strings are accepted).
func asInt(value any, def int) int {
	orDefault := func(v int) int {
		if v == 0 {
			return def
		}
		return v
	}
	switch v := value.(type) {
	case bool:
		return def
	case int:
		return orDefault(v)
	case int64:
		return orDefault(int(v))
	case int32:
		return orDefault(int(v))
	case float64: // encoding/json decodes every number as float64; accept integral values
		if v == math.Trunc(v) && !math.IsInf(v, 0) {
			return orDefault(int(v))
		}
		return def
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			return def
		}
		if n, err := strconv.Atoi(string(v)); err == nil {
			return orDefault(n)
		}
		return def
	case string:
		s := pyStrip(v)
		if s == "" {
			return def
		}
		for _, r := range s {
			if r < '0' || r > '9' {
				return def
			}
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return def
		}
		return orDefault(n)
	}
	return def
}

// Migrate applies registered migrators until data reaches targetVersion.
func Migrate(artifact string, data map[string]any, targetVersion int) (map[string]any, error) {
	raw, ok := data["schema_version"]
	if !ok {
		raw = 1
	}
	version := asInt(raw, 1)
	for version < targetVersion {
		migratorsMu.RLock()
		fn := migrators[migratorKey{artifact, version}]
		migratorsMu.RUnlock()
		if fn == nil {
			return nil, fmt.Errorf("no migrator registered for %s v%d", contracts.PyRepr(artifact), version)
		}
		data = fn(data)
		next, ok := data["schema_version"]
		if !ok {
			next = version + 1
		}
		newVersion := asInt(next, version+1)
		if newVersion <= version {
			return nil, fmt.Errorf("migrator for %s v%d did not advance the version",
				contracts.PyRepr(artifact), version)
		}
		version = newVersion
	}
	return data, nil
}
