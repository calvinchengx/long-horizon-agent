package agents

import "github.com/calvinchengx/long-horizon-agent/go/internal/coordination"

// The file-ownership map lives in the coordination package (python:
// lha.coordination.ownership); these names keep the Planner's API.

// Lead is the writer id of the serial lead engineer (owns all unassigned space).
const Lead = coordination.Lead

type (
	// InvalidPathError is a path that is absolute or escapes the repository root.
	InvalidPathError = coordination.InvalidPathError
	// FileOwnershipMap maps normalized, case-folded file paths to their single permitted writer.
	FileOwnershipMap = coordination.FileOwnershipMap
)

var (
	// NormalizePath normalizes a repo-relative path lexically (PurePosixPath semantics).
	NormalizePath = coordination.NormalizePath
	// IsShared reports whether a path is a shared file only the lead may write.
	IsShared = coordination.IsShared
	// NewFileOwnershipMap returns an empty map.
	NewFileOwnershipMap = coordination.NewFileOwnershipMap
	// WriterForItem is the writer id of the implementer that owns an item's write-set.
	WriterForItem = coordination.WriterForItem
	casefold      = coordination.Casefold
)
