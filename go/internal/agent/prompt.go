// Package agent is the agent plane: the inner loop that turns a model + tools into verified
// progress (python: lha.agent). It mirrors prompt.py (prompt construction), loop.py (one verified
// cycle of work), compaction.py (in-session context compaction) and runner.py (a local,
// non-Temporal mission runner).
package agent

import (
	"fmt"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/memory"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// Prompt construction is provider-agnostic: tools are described in text and the model is asked
// to reply with a single JSON action (use a tool) or a done signal. Native tool_calls are also
// honored when a backend returns them; the loop never sends tool definitions to the provider.
// The mission anchor — built from the IMMUTABLE mission spec, not the progress narrative — is
// recited every cycle to fight drift. Every prompt is byte-identical to the Python reference for
// the same inputs (spec/agent/prompts.json).

// ActionInstructions is the JSON action protocol of the built-in loop.
const ActionInstructions = "You act by replying with EXACTLY ONE JSON object and nothing else.\n" +
	`To use a tool: {"tool": "<name>", "arguments": { ... }}` + "\n" +
	`When the item is fully done: {"done": true, "summary": "<what you did>"}` + "\n" +
	"Take one action per reply. Prefer reading/searching before writing.\n" +
	"Signalling done does NOT mark the item done: the harness then runs the deterministic " +
	"checks, and only a green result counts. Do not edit files under .lha/ (harness-owned) and " +
	"do not modify or delete existing tests or test configuration."

// LeadDoneInstructions is for the lead only: the loop verifies when it signals done and feeds a
// failure back while turns remain. Without it, leads kept running tests (often unrelated suites)
// until out of turns.
const LeadDoneInstructions = "As soon as this item's acceptance checks pass, signal done instead of running more tests: " +
	"if the harness's checks then fail, you get the failure report while turns remain."

// EngineInstructions is the claude_code lead engine's prompt (Claude Code calls real tools, so
// there is no JSON protocol). The engine itself is not ported; the text is kept for parity.
const EngineInstructions = "Work this item in this session with the tools you have. When you believe it is done, call " +
	"the `verify` tool: it runs the mission's deterministic checks and this item's witnesses " +
	"exactly as the harness will after you stop. If it fails, fix the cause and verify again. " +
	"Stop when verify passes, or when you cannot make further progress, and end with a short " +
	"summary of what you changed.\n" +
	"Only a green result from the harness counts. Do not commit, push, or switch branches: the " +
	"harness commits verified work itself. Do not edit files under .lha/ (harness-owned) and do " +
	"not modify or delete existing tests or test configuration."

// correctiveTemplate is CORRECTIVE_INSTRUCTIONS with the format placeholder at %s.
const correctiveTemplate = "Your previous reply was not a valid action (%s). Reply again with EXACTLY ONE JSON " +
	`object and nothing else: {"tool": "<name>", "arguments": {...}} or ` +
	`{"done": true, "summary": "..."}. Keep it short.`

const (
	extraContextCap = 8000
	lastFailureCap  = 3000
	// CodeMapHeader heads the cycle-start code map (python: CODE_MAP_HEADER).
	CodeMapHeader = "Code map for this item (from ripwire: ranked definitions, callers and tests; it can miss " +
		"things, so read a file before you change it):"
	// CodeMapHardCap is the ceiling on the code map in the prompt (python: CODE_MAP_HARD_CAP).
	CodeMapHardCap = 16_000
	// MemoryHardCap is the absolute ceiling on the memory block, whatever budget is asked for.
	MemoryHardCap = memory.MemoryHardCap
	// MemoryHeader heads the rendered memory block.
	MemoryHeader    = memory.MemoryHeader
	decisionLineCap = 600
)

// LeadRole is the role line of the lead's system prompt.
const LeadRole = "You are the Lead Engineer. Make verified progress on ONE checklist item per cycle."

// RenderTools lists the tools as prompt text: "- name: description (args: {python dict repr})".
func RenderTools(specs []contracts.ToolSpec) string {
	lines := make([]string, 0, len(specs))
	for _, spec := range specs {
		var props any = map[string]any{}
		if spec.Parameters != nil {
			if p, ok := spec.Parameters["properties"]; ok {
				props = p
			}
		}
		lines = append(lines, fmt.Sprintf("- %s: %s (args: %s)", spec.Name, spec.Description, pyfmt.PyStr(props)))
	}
	if len(lines) == 0 {
		return "(no tools available)"
	}
	return strings.Join(lines, "\n")
}

// RenderDecisions is the recorded design decisions as a prompt section ("" when there are none).
func RenderDecisions(decisions []contracts.DecisionRecord) string {
	if len(decisions) == 0 {
		return ""
	}
	lines := []string{"Design decisions already recorded (stay consistent with them; if one must change, " +
		"record the new decision with record_decision):"}
	for _, record := range decisions {
		cycle := ""
		if record.CycleID != "" {
			cycle = "[" + record.CycleID + "] "
		}
		line := "- " + cycle + record.Decision + " — " + record.Rationale
		if record.AlternativesRejected != "" {
			line += " (rejected: " + record.AlternativesRejected + ")"
		}
		if len(record.Affected) > 0 {
			line += " (affects: " + strings.Join(record.Affected, ", ") + ")"
		}
		lines = append(lines, pyfmt.Head(line, decisionLineCap))
	}
	return strings.Join(lines, "\n")
}

// CorrectiveMessage is the user turn sent after an unparseable / truncated reply.
func CorrectiveMessage(reason string) contracts.ModelMessage {
	return contracts.ModelMessage{Role: "user", Content: fmt.Sprintf(correctiveTemplate, reason)}
}

// RenderCodeMap is the code-map section for a ripwire task bundle, "" when there is none (python:
// render_code_map).
func RenderCodeMap(output string) string {
	body := pyfmt.PyStrip(output)
	if body == "" {
		return ""
	}
	return clipLine(CodeMapHeader+"\n"+body, CodeMapHardCap)
}

func clipLine(line string, room int) string {
	if pyfmt.RuneLen(line) <= room {
		return line
	}
	return pyfmt.Head(line, max(0, room-15)) + " ...[clipped]"
}

// MemorySection is one titled section of retrieved memory (lines in relevance order).
type MemorySection = memory.MemorySection

// RenderMemoryBlock renders retrieved memory as header + titled sections within budgetChars
// (memory.RenderMemoryBlock; python: render_memory_block).
func RenderMemoryBlock(sections []MemorySection, budgetChars int, weights []float64) (string, error) {
	return memory.RenderMemoryBlock(sections, budgetChars, weights)
}

// PromptInput is everything BuildMessages reads.
type PromptInput struct {
	// AnchorText is optional caller context (progress, research, reflections).
	AnchorText string
	// MissionText is the immutable mission recitation (always first).
	MissionText string
	Snapshot    contracts.SituationSnapshot
	Item        contracts.ChecklistItem
	Specs       []contracts.ToolSpec
	// MemoryText is the already-budgeted memory block (capped again at MemoryHardCap).
	MemoryText string
	// CodeMapText is the rendered code map (RenderCodeMap), capped again at CodeMapHardCap.
	CodeMapText string
	// Engine selects the claude_code lead session prompt (tools are not listed, no JSON protocol).
	Engine bool
}

// BuildMessages builds the initial messages for one cycle: anchor recitation + active item +
// tools (python: build_messages).
func BuildMessages(in PromptInput) []contracts.ModelMessage {
	mission := pyfmt.PyStrip(in.MissionText)
	anchor := mission
	if anchor == "" {
		anchor = pyfmt.PyStrip(in.AnchorText)
	}
	extra := ""
	if mission != "" {
		extra = pyfmt.PyStrip(in.AnchorText)
	}
	if extra != "" && extra != anchor {
		if pyfmt.RuneLen(extra) > extraContextCap {
			extra = "...[earlier context trimmed]...\n" + pyfmt.Tail(extra, extraContextCap)
		}
		anchor = anchor + "\n\nContext:\n" + extra
	}
	var system string
	if in.Engine {
		system = anchor + "\n\n" + LeadRole + "\n\n" + EngineInstructions
	} else {
		system = anchor + "\n\n" + LeadRole + "\n\n" +
			"Available tools:\n" + RenderTools(in.Specs) + "\n\n" + ActionInstructions + "\n" + LeadDoneInstructions
	}
	commits := in.Snapshot.RecentCommits
	if len(commits) > 10 {
		commits = commits[:10]
	}
	recent := strings.Join(commits, "\n")
	if recent == "" {
		recent = "(none yet)"
	}
	item := in.Item
	user := "Active checklist item: [" + item.ID + "] " + item.Description + "\n\n"
	if len(in.Snapshot.Members) > 0 {
		user += "This workspace holds member repositories (git submodules), each a repository of " +
			"its own with its own tests and tooling; paths below are relative to the workspace " +
			"root: " + strings.Join(in.Snapshot.Members, ", ") + "\n\n"
	}
	if len(item.Witnesses) > 0 {
		listed := make([]string, len(item.Witnesses))
		for i, w := range item.Witnesses {
			listed[i] = "- " + w
			if command := verify.WitnessCommand(w); command != "" {
				listed[i] += " (run: " + command + ")"
			}
		}
		user += "This item is done only when ALL of these acceptance checks (witnesses) pass, in " +
			"addition to the mission's checks:\n" + strings.Join(listed, "\n") + "\n\n"
	}
	if item.LastFailure != "" {
		user += fmt.Sprintf("Previous attempt #%d FAILED verification:\n%s\n\n",
			item.Attempts, pyfmt.Tail(item.LastFailure, lastFailureCap))
	}
	if memory := pyfmt.PyStrip(in.MemoryText); memory != "" {
		user += clipLine(memory, MemoryHardCap) + "\n\n"
	}
	if codeMap := pyfmt.PyStrip(in.CodeMapText); codeMap != "" {
		user += clipLine(codeMap, CodeMapHardCap) + "\n\n"
	}
	if decisions := RenderDecisions(in.Snapshot.LastDecisions); decisions != "" {
		user += decisions + "\n\n"
	}
	finish := "signal done"
	if in.Engine {
		finish = "call verify"
	}
	user += "Recent commits:\n" + recent + "\n\nWork this item using the tools, then " + finish + "."
	return []contracts.ModelMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
}
