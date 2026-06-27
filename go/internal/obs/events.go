package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// ConfigureLogging installs the process-wide slog handler: human-readable text for development,
// or JSON (one object per line) for production / ingestion. Records carry the level and an ISO
// timestamp (the structlog processors of the reference). w defaults to os.Stderr when nil.
func ConfigureLogging(w io.Writer, jsonLogs bool) {
	if w == nil {
		w = os.Stderr
	}
	var h slog.Handler
	if jsonLogs {
		h = slog.NewJSONHandler(w, nil)
	} else {
		h = slog.NewTextHandler(w, nil)
	}
	slog.SetDefault(slog.New(h))
}

// Logger returns the default logger bound to name ("lha" when empty).
func Logger(name string) *slog.Logger {
	if name == "" {
		name = "lha"
	}
	return slog.Default().With("logger", name)
}

// Field is one key/value pair of event data. Event data keeps its insertion order (it is a
// Python dict in the reference, so the JSONL trace lists keys in the order they were recorded).
type Field struct {
	Key   string
	Value any
}

// F builds a Field.
func F(key string, value any) Field { return Field{key, value} }

// Fields is ordered event data. It marshals to a JSON object in order.
type Fields []Field

// Get returns the value recorded under key.
func (fs Fields) Get(key string) (any, bool) {
	for _, f := range fs {
		if f.Key == key {
			return f.Value, true
		}
	}
	return nil, false
}

// Map returns the data as a map.
func (fs Fields) Map() map[string]any {
	m := make(map[string]any, len(fs))
	for _, f := range fs {
		m[f.Key] = f.Value
	}
	return m
}

// MarshalJSON writes the fields as a JSON object in order (never null).
func (fs Fields) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range fs {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := marshalNoHTMLEscape(f.Key)
		if err != nil {
			return nil, err
		}
		v, err := marshalNoHTMLEscape(f.Value)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// UnmarshalJSON reads a JSON object, keeping the key order.
func (fs *Fields) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		*fs = Fields{}
		return nil
	}
	out := Fields{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			return err
		}
		out = append(out, Field{keyTok.(string), v})
	}
	*fs = out
	return nil
}

func marshalNoHTMLEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// TraceEvent is one observable step in a mission.
type TraceEvent struct {
	Kind      string `json:"kind"`
	MissionID string `json:"mission_id"`
	CycleID   string `json:"cycle_id"`
	Data      Fields `json:"data"`
}

// MarshalJSON is the compact form pydantic's model_dump_json produces (data is never null, and
// "<", ">", "&" are not HTML-escaped).
func (e TraceEvent) MarshalJSON() ([]byte, error) {
	type alias TraceEvent
	a := alias(e)
	if a.Data == nil {
		a.Data = Fields{}
	}
	return marshalNoHTMLEscape(a)
}

// TraceRecorder collects TraceEvents and mirrors them to a structured logger. It is safe for
// concurrent use.
type TraceRecorder struct {
	log    *slog.Logger
	mu     sync.Mutex
	events []TraceEvent
}

// NewTraceRecorder returns a recorder logging to logger (Logger("lha") when nil).
func NewTraceRecorder(logger *slog.Logger) *TraceRecorder {
	if logger == nil {
		logger = Logger("lha")
	}
	return &TraceRecorder{log: logger}
}

// Record redacts data, stores the event and logs it at info level with the event kind as the
// message. Duplicate keys keep the first position and the last value (dict semantics).
func (r *TraceRecorder) Record(kind, missionID, cycleID string, data ...Field) TraceEvent {
	safe := redactFields(data)
	event := TraceEvent{Kind: kind, MissionID: missionID, CycleID: cycleID, Data: safe}
	r.mu.Lock()
	r.events = append(r.events, event)
	logger := r.log
	r.mu.Unlock()
	if logger == nil {
		logger = Logger("lha")
	}
	attrs := make([]slog.Attr, 0, len(safe)+2)
	attrs = append(attrs, slog.String("mission_id", missionID), slog.String("cycle_id", cycleID))
	for _, f := range safe {
		attrs = append(attrs, slog.Any(f.Key, f.Value))
	}
	logger.LogAttrs(context.Background(), slog.LevelInfo, kind, attrs...)
	return event
}

func redactFields(data []Field) Fields {
	out := Fields{}
	index := map[string]int{}
	for _, f := range data {
		v := redactEntry(f.Key, f.Value)
		if i, ok := index[f.Key]; ok {
			out[i].Value = v
			continue
		}
		index[f.Key] = len(out)
		out = append(out, Field{f.Key, v})
	}
	return out
}

// Events returns a copy of the collected events.
func (r *TraceRecorder) Events() []TraceEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]TraceEvent(nil), r.events...)
}

// ToJSONL serializes the collected trace as newline-delimited JSON (for inspection / export).
func (r *TraceRecorder) ToJSONL() (string, error) {
	events := r.Events()
	lines := make([]string, 0, len(events))
	for _, e := range events {
		b, err := e.MarshalJSON()
		if err != nil {
			return "", err
		}
		lines = append(lines, string(b))
	}
	return strings.Join(lines, "\n"), nil
}
