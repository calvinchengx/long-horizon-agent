package durable

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
)

// Content-addressed object store for large-blob spillover (claim-check pattern)
// (python/src/lha/persistence/object_store.py). Blobs are stored as files named by their
// sha256 under the root; only the key is journaled in the durable history.
//
// Integrity, as in Python: keys are validated (^[0-9a-f]{64}$) before they touch the filesystem;
// the root is resolved to an absolute path at construction; Put is atomic (a temp file in the
// same directory + rename); Get re-hashes the bytes. Blobs are plaintext.

var keyRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// InvalidObjectKeyError: the key is not a lowercase hex sha256 digest.
type InvalidObjectKeyError struct{ Key string }

func (e *InvalidObjectKeyError) Error() string {
	return fmt.Sprintf("invalid object key %q: expected 64 lowercase hex chars", e.Key)
}

// ObjectCorruptError: the stored bytes do not hash to their key.
type ObjectCorruptError struct{ Key string }

func (e *ObjectCorruptError) Error() string {
	return fmt.Sprintf("object %s is corrupt (sha256 mismatch)", e.Key)
}

// ValidateKey returns key if it is a well-formed sha256 hex digest, else an error.
func ValidateKey(key string) (string, error) {
	if !keyRE.MatchString(key) {
		return "", &InvalidObjectKeyError{Key: key}
	}
	return key, nil
}

// ObjectStore stores and fetches blobs by content address.
type ObjectStore interface {
	Put(ctx context.Context, data []byte) (string, error)
	Get(ctx context.Context, key string) ([]byte, error)
}

// LocalFileObjectStore stores blobs as files under Root, named by sha256.
type LocalFileObjectStore struct {
	root string
}

// NewLocalFileObjectStore resolves root to an absolute path (expanding "~" and symlinks of the
// existing prefix, like Python's Path(root).expanduser().resolve()).
func NewLocalFileObjectStore(root string) (*LocalFileObjectStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("LocalFileObjectStore requires an explicit root directory")
	}
	return &LocalFileObjectStore{root: resolvePath(root)}, nil
}

// Root is the absolute store directory.
func (s *LocalFileObjectStore) Root() string { return s.root }

func resolvePath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	// Resolve symlinks of the longest existing prefix (the rest need not exist yet).
	rest := ""
	cur := abs
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Put stores data (atomically, deduplicated) and returns its key.
func (s *LocalFileObjectStore) Put(_ context.Context, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(s.root, key)
	if _, err := os.Stat(path); err == nil {
		return key, nil
	}
	tmp, err := os.CreateTemp(s.root, "."+key[:12]+".*.tmp")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(name, path); err != nil {
		return "", err
	}
	ok = true
	return key, nil
}

// Get fetches the bytes for key, verifying their hash.
func (s *LocalFileObjectStore) Get(_ context.Context, key string) ([]byte, error) {
	if _, err := ValidateKey(key); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.root, key))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != key {
		return nil, &ObjectCorruptError{Key: key}
	}
	return data, nil
}

// PruneResult is what PruneObjects deleted (or would delete) and what it kept.
type PruneResult struct {
	Count int
	Bytes int64
	Kept  int
}

// PruneObjects deletes the objects under root not modified for olderThanDays days (dryRun: only
// counts them). Only files named by a sha256 key are considered; a missing root is empty.
// Deleting an object a live workflow history still refers to breaks that mission, so the age must
// exceed the longest mission plus Temporal's history retention (python: prune_objects).
func PruneObjects(root string, olderThanDays int, dryRun bool) (PruneResult, error) {
	if olderThanDays < 1 {
		return PruneResult{}, errors.New("older_than_days must be >= 1")
	}
	cutoff := time.Now().Add(-time.Duration(olderThanDays) * 24 * time.Hour)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return PruneResult{}, nil
	}
	if err != nil {
		return PruneResult{}, err
	}
	var out PruneResult
	for _, e := range entries {
		if _, err := ValidateKey(e.Name()); err != nil || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !info.ModTime().Before(cutoff) {
			out.Kept++
			continue
		}
		out.Count++
		out.Bytes += info.Size()
		if !dryRun {
			if err := os.Remove(filepath.Join(root, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return out, err
			}
		}
	}
	return out, nil
}

// SweepObjects deletes ClaimCheck objects untouched for LHA_OBJECT_RETENTION_DAYS (nil when 0).
// It runs once when a worker starts, so a long-lived deployment's store stops growing without an
// operator's `lha objects prune`; the result is logged as objects_pruned (python: sweep_objects).
func SweepObjects(settings *config.Settings) (*PruneResult, error) {
	days := settings.ObjectRetentionDays
	if days <= 0 {
		return nil, nil
	}
	result, err := PruneObjects(settings.ObjectStoreRoot, days, false)
	if err != nil {
		return nil, err
	}
	obs.Logger("lha.durable").Info("objects_pruned", "root", settings.ObjectStoreRoot, "older_than_days", days,
		"deleted", result.Count, "bytes", result.Bytes, "kept", result.Kept)
	return &result, nil
}
