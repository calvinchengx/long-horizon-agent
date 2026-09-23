package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// The .lha/decisions.ndjson format: a SHA-256 hash chain of DecisionRecords, shared with the
// Python implementation (python/src/lha/coordination/decision_log.py; cases in
// spec/coordination/decision_chain.json).
//
//   - A chained line is {"prev": P, "hash": H, "record": R} with
//     H = hex(sha256(P + "\n" + canonical(R))), canonical = sorted keys, "," and ":" separators,
//     non-ASCII left raw. The first P is GenesisHash.
//   - A legacy line (a bare DecisionRecord, written before the chain existed) may only appear in a
//     leading prefix; it advances the running hash as if it were chained, so the first chained
//     line seals the prefix. A legacy line after a chained one is tampering.
//   - Lines are split on "\n" only; a final line without its "\n" is a torn (incomplete) write.

// GenesisHash is the prev of the first chained record.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// DecisionChainError reports a committed decision log whose hash chain does not verify.
type DecisionChainError struct{ Msg string }

func (e *DecisionChainError) Error() string { return e.Msg }

// errCorruptDecisionLog is a non-final unreadable line (not explainable by a torn write).
var errCorruptDecisionLog = errors.New("unreadable record")

// ChainVerification is the result of VerifyDecisionChain.
type ChainVerification struct {
	OK       bool
	Checked  int // records that verified (legacy prefix lines included)
	Problem  string
	TornTail bool
	Legacy   int // of Checked, the leading legacy (pre-chain) lines
}

// DecisionChain is what ParseDecisionChain found.
type DecisionChain struct {
	Records     []contracts.DecisionRecord
	LastHash    string // the hash the next appended record chains from
	TornTail    bool
	IntactBytes int // length of the prefix holding only intact lines
	Legacy      int
}

// CanonicalJSON renders a parsed JSON value (from a decoder with UseNumber) like Python's
// json.dumps(v, sort_keys=True, separators=(",", ":"), ensure_ascii=False).
func CanonicalJSON(v any) (string, error) {
	var b strings.Builder
	if err := writePyJSON(&b, v, ",", ":", true); err != nil {
		return "", err
	}
	return b.String(), nil
}

// ChainHash is hex(sha256(prev + "\n" + canonical(record))).
func ChainHash(prev string, record any) (string, error) {
	canon, err := CanonicalJSON(record)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(prev + "\n" + canon))
	return hex.EncodeToString(sum[:]), nil
}

// recordValue is record as the generic JSON value the hash is computed over (field order is
// irrelevant: the canonical form sorts keys).
func recordValue(d contracts.DecisionRecord) map[string]any {
	affected := make([]any, 0, len(d.Affected))
	for _, a := range d.Affected {
		affected = append(affected, a)
	}
	return map[string]any{
		"decision":              d.Decision,
		"rationale":             d.Rationale,
		"alternatives_rejected": d.AlternativesRejected,
		"affected":              affected,
		"cycle_id":              d.CycleID,
	}
}

// EncodeDecisionLink returns the envelope line (without "\n") chaining record after prev, and its
// hash. The bytes match Python's json.dumps({"prev", "hash", "record"}, ensure_ascii=False).
func EncodeDecisionLink(prev string, d contracts.DecisionRecord) (string, string, error) {
	digest, err := ChainHash(prev, recordValue(d))
	if err != nil {
		return "", "", err
	}
	var b strings.Builder
	b.WriteString(`{"prev": `)
	writePyString(&b, prev)
	b.WriteString(`, "hash": `)
	writePyString(&b, digest)
	b.WriteString(`, "record": {"decision": `)
	writePyString(&b, d.Decision)
	b.WriteString(`, "rationale": `)
	writePyString(&b, d.Rationale)
	b.WriteString(`, "alternatives_rejected": `)
	writePyString(&b, d.AlternativesRejected)
	b.WriteString(`, "affected": [`)
	for i, a := range d.Affected {
		if i > 0 {
			b.WriteString(", ")
		}
		writePyString(&b, a)
	}
	b.WriteString(`], "cycle_id": `)
	writePyString(&b, d.CycleID)
	b.WriteString("}}")
	return b.String(), digest, nil
}

func decodeLine(line []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

func isEnvelope(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	_, hasHash := m["hash"]
	_, hasRecord := m["record"]
	return m, hasHash && hasRecord
}

// decisionRecordFrom validates a record object the way pydantic validates a DecisionRecord:
// decision and rationale are required strings; the other fields are optional and typed.
func decisionRecordFrom(v any) (contracts.DecisionRecord, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return contracts.DecisionRecord{}, errors.New("record is not a JSON object")
	}
	str := func(key string, required bool) (string, error) {
		raw, present := m[key]
		if !present {
			if required {
				return "", fmt.Errorf("record is missing %q", key)
			}
			return "", nil
		}
		s, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("record %q is not a string", key)
		}
		return s, nil
	}
	var d contracts.DecisionRecord
	var err error
	if d.Decision, err = str("decision", true); err != nil {
		return d, err
	}
	if d.Rationale, err = str("rationale", true); err != nil {
		return d, err
	}
	if d.AlternativesRejected, err = str("alternatives_rejected", false); err != nil {
		return d, err
	}
	if d.CycleID, err = str("cycle_id", false); err != nil {
		return d, err
	}
	d.Affected = []string{}
	if raw, present := m["affected"]; present {
		list, ok := raw.([]any)
		if !ok {
			return d, errors.New(`record "affected" is not a list`)
		}
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return d, errors.New(`record "affected" has a non-string entry`)
			}
			d.Affected = append(d.Affected, s)
		}
	}
	return d, nil
}

// ParseDecisionChain parses a log (chained and/or legacy lines), skipping a torn final line.
// Records are validated but hashes are NOT checked (VerifyDecisionChain does that).
func ParseDecisionChain(data []byte) (DecisionChain, error) {
	chain := DecisionChain{LastHash: GenesisHash, Records: []contracts.DecisionRecord{}}
	parts := bytes.Split(data, []byte("\n"))
	tail := parts[len(parts)-1]
	parts = parts[:len(parts)-1]
	offset := 0
	for index, raw := range parts {
		if len(bytes.TrimSpace(raw)) > 0 {
			record, digest, legacy, err := parseDecisionLine(raw, chain.LastHash)
			if err != nil {
				if index == len(parts)-1 && len(bytes.TrimSpace(tail)) == 0 {
					chain.TornTail = true
					return chain, nil
				}
				return chain, fmt.Errorf("%w on line %d: %v", errCorruptDecisionLog, index+1, err)
			}
			if legacy {
				chain.Legacy++
			}
			chain.Records = append(chain.Records, record)
			chain.LastHash = digest
		}
		offset += len(raw) + 1
		chain.IntactBytes = offset
	}
	if len(bytes.TrimSpace(tail)) > 0 {
		chain.TornTail = true
	}
	return chain, nil
}

func parseDecisionLine(raw []byte, running string) (contracts.DecisionRecord, string, bool, error) {
	if !utf8.Valid(raw) {
		return contracts.DecisionRecord{}, "", false, errors.New("invalid UTF-8")
	}
	v, err := decodeLine(raw)
	if err != nil {
		return contracts.DecisionRecord{}, "", false, err
	}
	if env, ok := isEnvelope(v); ok {
		record, err := decisionRecordFrom(env["record"])
		if err != nil {
			return record, "", false, err
		}
		digest, ok := env["hash"].(string)
		if !ok {
			digest = fmt.Sprint(env["hash"])
		}
		return record, digest, false, nil
	}
	record, err := decisionRecordFrom(v)
	if err != nil {
		return record, "", true, err
	}
	digest, err := ChainHash(running, v)
	return record, digest, true, err
}

// VerifyDecisionChain recomputes the hash chain over the intact lines of data. A torn final line
// is reported (TornTail) but is not itself a failure.
func VerifyDecisionChain(data []byte) ChainVerification {
	chain, err := ParseDecisionChain(data)
	if err != nil {
		return ChainVerification{Problem: "decision log: " + err.Error()}
	}
	out := ChainVerification{OK: true, TornTail: chain.TornTail}
	prev := GenesisHash
	chained := false
	for number, raw := range bytes.Split(data[:chain.IntactBytes], []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		fail := func(problem string) ChainVerification {
			out.OK = false
			out.Problem = fmt.Sprintf("line %d: %s", number+1, problem)
			return out
		}
		v, _ := decodeLine(raw) // ParseDecisionChain proved it parses
		env, ok := isEnvelope(v)
		if !ok {
			if chained {
				return fail("unchained record after the chain began")
			}
			prev, _ = ChainHash(prev, v)
			out.Legacy++
			out.Checked++
			continue
		}
		chained = true
		if p, ok := env["prev"].(string); !ok || p != prev {
			return fail("prev-hash mismatch")
		}
		record, ok := env["record"].(map[string]any)
		if !ok {
			return fail("hash mismatch")
		}
		digest, err := ChainHash(prev, record)
		if h, ok := env["hash"].(string); err != nil || !ok || h != digest {
			return fail("hash mismatch")
		}
		prev = digest
		out.Checked++
	}
	return out
}

// --- Python-compatible JSON writing ------------------------------------------------------------

// writePyString writes s as Python's json.dumps(ensure_ascii=False) does: only '"', '\\' and
// control characters are escaped (\n \r \t \b \f short, the rest as \u00XX); everything else,
// including '<', '>', '&', U+2028 and U+2029, is written raw.
func writePyString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

func writePyJSON(b *strings.Builder, v any, itemSep, keySep string, sortKeys bool) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writePyString(b, x)
	case json.Number:
		b.WriteString(x.String())
	case float64:
		b.WriteString(pyFloatRepr(x))
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteString(itemSep)
			}
			if err := writePyJSON(b, item, itemSep, keySep, sortKeys); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		if sortKeys {
			sort.Strings(keys) // UTF-8 byte order == code point order, as Python sorts str
		}
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(itemSep)
			}
			writePyString(b, k)
			b.WriteString(keySep)
			if err := writePyJSON(b, x[k], itemSep, keySep, sortKeys); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value %T", v)
	}
	return nil
}
