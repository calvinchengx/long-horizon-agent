package safety

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// Loops a mutation can keep from terminating: classifyCurl skipping an option's value,
// findExecCommands resuming after an -exec group, parseIPv6 filling a "::" gap, and hex16 (an
// IPv6 address with an IPv4 tail). Each call runs under a deadline, and a missed deadline ends
// the PROCESS: several of these loops append on every pass, so a goroutine left spinning after a
// mere test failure allocates gigabytes a second until the test binary ends (on a CI runner,
// enough to take the machine down); exiting kills it at once and still counts as a failed run.
// go test runs files in name order, so this runs before the tests that would otherwise hang first.
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
		{"IPv6 with a :: gap", func() bool { // parseIPv6: the skipped-hextet loop appends per pass
			a, err := parseIPAddress("::1")
			return err == nil && a == ipAddr{v6: true, lo: 1}
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
		case <-time.After(300 * time.Millisecond): // the calls take microseconds; a spin allocates GB/s
			fmt.Fprintf(os.Stderr, "FAIL: TestLoopsTerminate: %s: did not return\n", c.name)
			os.Exit(1) // not t.Fatal: the loop must stop allocating now, not when the binary ends
		}
	}
}
