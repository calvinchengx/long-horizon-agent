package memory

import (
	"fmt"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// The memory block of the lead's prompt (python: lha.agent.prompt.render_memory_block). It lives
// here so the memory plane can render without importing the agent package; agent re-exports it.

const (
	// MemoryHardCap is the absolute ceiling on the memory block, whatever budget is asked for.
	MemoryHardCap = 20_000
	// MemoryHeader heads the rendered memory block.
	MemoryHeader = "Relevant memory (retrieved from earlier cycles, skills and the repository; it may be " +
		"stale, so verify before relying on it):"
	minLine = 40
)

// MemorySection is one titled section of retrieved memory (lines in relevance order).
type MemorySection struct {
	Title string
	Lines []string
}

func clipLine(line string, room int) string {
	if pyfmt.RuneLen(line) <= room {
		return line
	}
	return pyfmt.Head(line, max(0, room-15)) + " ...[clipped]"
}

// RenderMemoryBlock renders retrieved memory as header + titled sections within budgetChars.
// Each section gets a share of the budget proportional to its weight (nil = equal); what a section
// leaves unused rolls over to the ones after it. Lines are kept in order and clipped, never
// reordered; empty sections are omitted; with nothing to show the result is "". weights must have
// one entry per section (else an error, as Python's ValueError).
func RenderMemoryBlock(sections []MemorySection, budgetChars int, weights []float64) (string, error) {
	budget := min(max(0, budgetChars), MemoryHardCap)
	var live []MemorySection
	for _, s := range sections {
		if len(s.Lines) > 0 {
			live = append(live, s)
		}
	}
	if len(live) == 0 || budget < pyfmt.RuneLen(MemoryHeader)+minLine {
		return "", nil
	}
	raw := weights
	if raw == nil {
		raw = make([]float64, len(sections))
		for i := range raw {
			raw[i] = 1.0
		}
	}
	if len(raw) != len(sections) {
		return "", fmt.Errorf("render_memory_block: one weight per section is required")
	}
	liveWeights := []float64{}
	for i, s := range sections {
		if len(s.Lines) > 0 {
			liveWeights = append(liveWeights, raw[i])
		}
	}
	remaining := budget - pyfmt.RuneLen(MemoryHeader)
	out := []string{MemoryHeader}
	for index, section := range live {
		share := 0.0
		for _, w := range liveWeights[index:] {
			share += w
		}
		if share == 0 {
			share = 1.0
		}
		allowance := int(float64(remaining) * (liveWeights[index] / share)) // python int(): truncation
		head := section.Title + ":"
		if allowance < pyfmt.RuneLen(head)+1+minLine {
			continue
		}
		chunk := []string{head}
		used := pyfmt.RuneLen(head) + 1
		for _, line := range section.Lines {
			room := allowance - used - 1
			if room < minLine {
				break
			}
			clipped := clipLine(line, room)
			chunk = append(chunk, clipped)
			used += pyfmt.RuneLen(clipped) + 1
		}
		if len(chunk) > 1 {
			out = append(out, strings.Join(chunk, "\n"))
			remaining -= used + 1
		}
	}
	if len(out) > 1 {
		return strings.Join(out, "\n"), nil
	}
	return "", nil
}
