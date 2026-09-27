package contracts

import (
	"encoding/json"
	"testing"
)

func TestOrderedMapKeepsInsertionOrder(t *testing.T) {
	m := NewOrderedMap("z", 1, "a", 1.0, "m", map[string]any{"y": 2, "b": []any{0.5, 1e16, 2.5e-5, 1.5e-7}})
	m.Set("z", "again") // keeps its position
	m.Set("new", nil)
	m.Delete("m")
	m.Set("m", NewOrderedMap("q", true, "c", false))
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"z":"again","a":1.0,"new":null,"m":{"q":true,"c":false}}`; string(data) != want {
		t.Fatalf("got  %s\nwant %s", data, want)
	}
	data, _ = json.Marshal(NewOrderedMap("f", []any{0.5, 1e16, 2.5e-5, 1.5e-7, 100.0}, "plain", map[string]any{"y": 2, "b": 1}))
	if want := `{"f":[0.5,1e+16,0.000025,1.5e-7,100.0],"plain":{"b":1,"y":2}}`; string(data) != want {
		t.Fatalf("got  %s\nwant %s", data, want)
	}
}

func TestOrderedMapRoundTripsBytes(t *testing.T) {
	in := `{"b":1,"a":{"z":[1,2.50,{"y":null,"x":"s"}],"c":1.0},"n":1e-7}`
	var m OrderedMap
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(&m)
	if string(out) != in { // json.Number keeps each number's text
		t.Fatalf("got %s", out)
	}
	if v, _ := m.Get("b"); v.(json.Number) != "1" || m.Len() != 3 {
		t.Fatal(m.Keys)
	}
	if f, ok := AsFloat(m.Value("n")); !ok || f != 1e-7 {
		t.Fatal(f)
	}
	var nilMap *OrderedMap
	if nilMap.Len() != 0 || nilMap.Value("x") != nil || len(nilMap.Plain()) != 0 {
		t.Fatal("nil map")
	}
	var e EventRecord
	if err := json.Unmarshal([]byte(`{"kind":"k","cycle_id":"c","payload":null,"payload_ref":null}`), &e); err != nil {
		t.Fatal(err)
	}
	if out, _ := json.Marshal(e); string(out) != `{"kind":"k","cycle_id":"c","payload":{},"payload_ref":null}` {
		t.Fatal(string(out))
	}
}
