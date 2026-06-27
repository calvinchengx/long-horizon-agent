package safety

import "testing"

// Expected values are the Python reference's normalize_host / is_ambiguous_idn output; the cases
// exercise the CONTEXTJ / CONTEXTO rules, the Bidi Rule, A-label validation and the IDNA 2003
// (nameprep) path.
func TestIDNAParity(t *testing.T) {
	cases := []struct {
		host, normalized string
		ambiguous        bool
	}{
		{"\u0628\u200c\u0628.x", "xn--ngba799q.x", true},
		{"a\u200cb.x", "", true},
		{"\u0628\u064b\u200c\u064b\u0628.x", "xn--ngba8ha8704a.x", true},
		{"\u0628\u200c.x", "", true},
		{"\u200cx.x", "", true},
		{"\u200dx.x", "", true},
		{"\u0628\u200c\u0627.x", "xn--mgbb899q.x", true},
		{"\u0627\u200c\u0628.x", "", true},
		{"\u00b7l.x", "", true},
		{"l\u00b7.x", "", true},
		{"\u0375\u03b1.x", "xn--wva4j.x", false},
		{"\u0375a.x", "", true},
		{"\u03b1\u0375.x", "", true},
		{"\u05d0\u05f3.x", "xn--4db4e.x", false},
		{"\u05f3.x", "", true},
		{"\u30fb\u30a2.x", "xn--cckyj.x", false},
		{"a\u30fb.x", "", true},
		{"\u30fb.x", "", true},
		{"\u0661\u0662.x", "", true},
		{"\u06f1\u06f2.x", "xn--embc.x", false},
		{"\u05d0\u0661.x", "xn--4db40a.x", false},
		{"\u05d01.x", "xn--1-zhc.x", false},
		{"\u05d0\u06611.x", "", false},
		{"1\u05d0.x", "", false},
		{"\u05d0\u0591.x", "xn--ccb9j.x", false},
		{"a\u05d0.x", "", false},
		{"a\u0661.x", "", true},
		{"\u05d0a.x", "", false},
		{"\u05d0-\u05d1.x", "xn----zhce.x", false},
		{"\u0627\u0661\u0662.x", "xn--mgb0jd.x", false},
		{"a1.\u05d0.x", "a1.xn--4db.x", false},
		{"\u05d0.a.x", "xn--4db.a.x", false},
		{"\u05d0!.x", "", false},
		{"a\u0300.\u05d0.x", "xn--0ca.xn--4db.x", false},
		{"\u04c0.de", "xn--s5a.de", false},
		{"\u0080.de", "", false},
		{"\u2488.de", "", true},
		{"\u2024.de", "", true},
		{"\ufa70.de", "xn--7hq.de", true},
		{"\U0001f600.de", "", true},
		{"\u0915\u094d\u200c.de", "xn--11b6iv14e.de", true},
		{"\u200d.de", "", false},
		{"a\u0600.x", "", true},
		{"\u0600.x", "", true},
		{"xn--.de\u00fc", "", true},
		{"\u00fc.xn--a.de", "", true},
		{"\u00fc.xn--ab-.de", "", true},
		{"\u00fc.xn--9.de", "", true},
		{"\u00fc.xn--zzzzzzzzzzz.de", "", true},
		{"\u00fc.XN--BCHER-KVA.de", "xn--tda.xn--bcher-kva.de", false},
		{"\u0661\u06f1.x", "", true},
		{"\u05d0\u05d1\u0300.x", "xn--ksa35lda.x", false},
		{"\u0628\u0300\u200c.x", "", false},
		{"\u0915\u200d.x", "", true},
	}
	for _, c := range cases {
		if got := NormalizeHost(c.host); got != c.normalized {
			t.Errorf("NormalizeHost(%+q) = %q, want %q", c.host, got, c.normalized)
		}
		if got := IsAmbiguousIDN(c.host); got != c.ambiguous {
			t.Errorf("IsAmbiguousIDN(%+q) = %v, want %v", c.host, got, c.ambiguous)
		}
	}
}
