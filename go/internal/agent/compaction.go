package agent

import (
	"context"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// In-session context compaction (python: lha.agent.compaction). When a multi-turn cycle's
// message list grows, the older turns are summarized into one compact, state-carrying note (task
// / decisions / discoveries incl. failed approaches / next steps), keeping the system message, the
// original task message and the most recent turns verbatim. The summarizer sees the NEWEST part
// of the older transcript (each message clipped), and the kept tail never starts with an orphaned
// tool observation whose action was summarized.

// SummaryInstructions is the summarizer's system prompt.
const SummaryInstructions = "Summarize the conversation so far into a compact, state-carrying note. Preserve: the task, " +
	"key decisions (and why), discoveries INCLUDING failed approaches, and next steps. Be terse."

const (
	transcriptCap = 12000
	perMessageCap = 2000
)

func isObservation(m contracts.ModelMessage) bool {
	return m.Role == "tool" || (m.Role == "user" && strings.HasPrefix(m.Content, "OBSERVATION"))
}

func clipMiddle(text string, limit int) string {
	if pyfmt.RuneLen(text) <= limit {
		return text
	}
	half := limit / 2
	return pyfmt.Head(text, half) + "\n...[clipped]...\n" + pyfmt.Tail(text, half)
}

// CompactMessages returns system + task + summary-of-older + the last ~keepLast turns, or
// messages unchanged when there is nothing to summarize (python: compact_messages; the Python
// defaults are keepLast=4, keepTask=true).
func CompactMessages(ctx context.Context, model contracts.ModelProvider, messages []contracts.ModelMessage, keepLast int, keepTask bool) ([]contracts.ModelMessage, error) {
	var system, body []contracts.ModelMessage
	for _, m := range messages {
		if m.Role == "system" {
			if len(system) == 0 {
				system = append(system, m)
			}
		} else {
			body = append(body, m)
		}
	}
	var task []contracts.ModelMessage
	if keepTask && len(body) > 0 && body[0].Role == "user" {
		task = body[:1]
	}
	rest := body[len(task):]
	if len(rest) <= keepLast {
		return messages, nil
	}
	keepLast = max(0, keepLast)
	split := len(rest) - keepLast
	for split > 0 && split < len(rest) && isObservation(rest[split]) {
		split--
	}
	toSummarize, recent := rest[:split], rest[split:]
	if len(toSummarize) == 0 {
		return messages, nil
	}
	lines := make([]string, len(toSummarize))
	for i, m := range toSummarize {
		lines[i] = m.Role + ": " + clipMiddle(m.Content, perMessageCap)
	}
	transcript := strings.Join(lines, "\n")
	if pyfmt.RuneLen(transcript) > transcriptCap {
		transcript = "...[older turns trimmed]...\n" + pyfmt.Tail(transcript, transcriptCap)
	}
	result, err := model.Complete(ctx, []contracts.ModelMessage{
		{Role: "system", Content: SummaryInstructions},
		{Role: "user", Content: transcript},
	}, nil, 0)
	if err != nil {
		return nil, err
	}
	summary := contracts.ModelMessage{Role: "user", Content: "[compacted summary of earlier turns]\n" + pyfmt.Head(result.Text, 4000)}
	out := append([]contracts.ModelMessage{}, system...)
	out = append(out, task...)
	out = append(out, summary)
	return append(out, recent...), nil
}
