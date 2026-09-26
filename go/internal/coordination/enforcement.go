package coordination

import (
	"context"
	"strings"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Enforcing the FileOwnershipMap at the tool call, and again at the git layer (python:
// lha.coordination.enforcement). Two layers, because neither alone is enough:
//
//   - OwnershipGuard wraps a ToolDispatcher. A mutating tool that names workspace paths
//     (ToolSpec.PathArgs, e.g. write_file) is refused — before it runs — when the path is not
//     writable by the agent's writer identities. The shell tool names no paths, so it cannot be
//     checked here.
//   - ChangedPaths + FileOwnershipMap.ViolationsAny check what a writer actually changed on its
//     branch (git diff), which catches writes made through the shell.

// Harness-owned paths are never part of a writer's change set (and never merged).
var excludedFromChanges = []string{".lha"}

// OwnershipGuard is a ToolDispatcher that refuses mutating path writes outside the writer's
// files. Writers are the identities the agent acts as: an implementer is just its own id; the
// serial lead working an item is {"lead", <that item's implementer id>}. Both the map and the
// identities can be swapped between cycles (Update).
type OwnershipGuard struct {
	inner     contracts.ToolDispatcher
	leaseTool bool // the agent has request_lease: say so in refusals

	mu        sync.Mutex
	ownership *FileOwnershipMap
	writers   []string
}

var (
	_ contracts.ToolDispatcher = (*OwnershipGuard)(nil)
	_ contracts.EventDrainer   = (*OwnershipGuard)(nil)
)

// NewOwnershipGuard wraps inner. The guard reads ownership live (a lease granted into the same
// map is writable at once); leaseTool says the agent can call request_lease.
func NewOwnershipGuard(inner contracts.ToolDispatcher, ownership *FileOwnershipMap, writers []string, leaseTool bool) *OwnershipGuard {
	if ownership == nil {
		ownership = NewFileOwnershipMap()
	}
	return &OwnershipGuard{inner: inner, ownership: ownership, writers: append([]string{}, writers...), leaseTool: leaseTool}
}

// Writers are the identities the guard currently permits.
func (g *OwnershipGuard) Writers() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string{}, g.writers...)
}

// Update swaps the map and the identities (between cycles).
func (g *OwnershipGuard) Update(ownership *FileOwnershipMap, writers []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ownership == nil {
		ownership = NewFileOwnershipMap()
	}
	g.ownership = ownership
	g.writers = append([]string{}, writers...)
}

// Inner is the wrapped dispatcher.
func (g *OwnershipGuard) Inner() contracts.ToolDispatcher { return g.inner }

// Specs are the wrapped dispatcher's specs.
func (g *OwnershipGuard) Specs() []contracts.ToolSpec { return g.inner.Specs() }

// DrainEvents forwards the wrapped dispatcher's gate events (tool_approval etc.), so wrapping
// never hides the approval audit trail from the agent loop.
func (g *OwnershipGuard) DrainEvents() []contracts.EventRecord {
	if drainer, ok := g.inner.(contracts.EventDrainer); ok {
		return append([]contracts.EventRecord{}, drainer.DrainEvents()...)
	}
	return []contracts.EventRecord{}
}

// Dispatch refuses a mutating path write the writers may not make; everything else is delegated.
func (g *OwnershipGuard) Dispatch(ctx context.Context, call contracts.ToolCall, tctx contracts.ToolContext) contracts.ToolResult {
	for _, spec := range g.inner.Specs() {
		if spec.Name != call.Name {
			continue
		}
		if spec.Mutating {
			for _, name := range spec.PathArgs {
				if value, ok := call.Arguments[name].(string); ok && value != "" {
					if refusal := g.Refusal(value); refusal != "" {
						return contracts.Failure(refusal)
					}
				}
			}
		}
		break
	}
	return g.inner.Dispatch(ctx, call, tctx)
}

// Refusal is why the current writers may not write p ("" if they may).
func (g *OwnershipGuard) Refusal(p string) string {
	g.mu.Lock()
	ownership, writers := g.ownership, g.writers
	g.mu.Unlock()
	if ownership.PermitsAny(writers, p) {
		return ""
	}
	who := strings.Join(writers, " / ")
	norm, err := NormalizePath(p)
	if err != nil {
		return "ownership: " + who + " may not write " + contracts.PyRepr(p) + ": " + err.Error()
	}
	var reason string
	if isSharedNorm(norm) {
		reason = "it is a shared file (build manifest, lockfile, package entry point, ...) " +
			"that only the lead writes"
	} else if owner := ownership.OwnerOf(norm); owner == "" {
		reason = "it is outside your write-set (unassigned files belong to the lead)"
	} else {
		reason = "it is owned by " + contracts.PyRepr(owner)
	}
	advice := "if you really need this file, stop and say so in your summary (a lease request) " +
		"instead of writing it"
	if g.leaseTool {
		advice = "if you really need this file, call request_lease with the path and why; write " +
			"it only if the lease is granted"
	}
	return "ownership: " + who + " may not write " + contracts.PyRepr(norm) + ": " + reason +
		". Write only the files you own; " + advice + "."
}

// ChangedPaths are the repo-relative paths that differ between base and head (.lha/ excluded).
func ChangedPaths(ctx context.Context, workdir, base, head string) ([]string, error) {
	if base == "" || head == "" || base == head {
		return []string{}, nil
	}
	args := []string{"diff", "--name-only", "--no-renames", base + ".." + head, "--", "."}
	for _, p := range excludedFromChanges {
		args = append(args, ":(exclude)"+p)
	}
	out, err := state.RunGit(ctx, workdir, args...)
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			paths = append(paths, strings.TrimRight(line, "\r"))
		}
	}
	return paths, nil
}

// EffectiveOwnership is ownership with every finished writer's files released to the lead (a
// copy; ownership is not changed).
func EffectiveOwnership(ownership *FileOwnershipMap, doneWriters []string) *FileOwnershipMap {
	effective := ownership.Clone()
	for _, writer := range doneWriters {
		if writer != Lead {
			effective.Release(writer)
		}
	}
	return effective
}
