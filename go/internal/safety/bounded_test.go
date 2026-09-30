package safety

import (
	"testing"
	"time"
)

// Loops a mutation can keep from terminating: classifyCurl skipping an option's value,
// findExecCommands resuming after an -exec group, and hex16 (an IPv6 address with an IPv4 tail). Each call runs under a deadline so such a mutant fails
// fast instead of hanging the whole run: go test runs files in name order, so this runs before
// the tests that would otherwise hang first.
func TestLoopsTerminate(t *testing.T) {
	for _, c := range []struct {
		name string
		run  func() bool
	}{
		{"curl option with a separate value", func() bool {
			reason, _ := ClassifyCommand([]string{"curl", "-o", "f", "u"})
			return reason == ""
		}},
		{"find -exec followed by more predicates", func() bool { // findExecCommands: i = end, then i++
			reason, _ := ClassifyCommand([]string{"find", "-exec", "echo", "{}", ";", "-name", "x"})
			return reason == ""
		}},
		{"IPv6 with an IPv4 tail", func() bool {
			a, err := parseIPAddress("::ffff:1.2.3.4")
			return err == nil && a == ipAddr{v6: true, lo: 0xffff01020304}
		}},
	} {
		done := make(chan bool)
		go func() { done <- c.run() }()
		select {
		case ok := <-done:
			if !ok {
				t.Errorf("%s: wrong answer", c.name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: did not return", c.name)
		}
	}
}
