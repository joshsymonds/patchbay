package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	transcriptKindClaude        = "claude"
	transcriptKindPi            = "pi"
	duplicateMinimumFiles       = 2
	transcriptInventoryFileMode = 0o600
)

// TranscriptInventory is a frozen manifest of transcript files discovered under
// the two transcript roots.
type TranscriptInventory struct {
	GeneratedAt string                         `json:"generated_at"`
	Roots       TranscriptInventoryRoots       `json:"roots"`
	Files       []TranscriptInventoryFile      `json:"files"`
	Duplicates  []TranscriptInventoryDuplicate `json:"duplicates"`
}

// TranscriptInventoryRoots records the absolute roots used to create a manifest.
type TranscriptInventoryRoots struct {
	ClaudeProjects string `json:"claude_projects"`
	PISessions     string `json:"pi_sessions"`
}

// TranscriptInventoryFile describes one JSONL file relative to its root.
type TranscriptInventoryFile struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Size    int64  `json:"size"`
	MTimeNS int64  `json:"mtime_ns"`
	SHA256  string `json:"sha256"`
}

// TranscriptInventoryDuplicate groups byte-identical files by their digest.
type TranscriptInventoryDuplicate struct {
	SHA256    string   `json:"sha256"`
	Canonical string   `json:"canonical"`
	Others    []string `json:"others"`
}

// InventoryTranscripts writes an atomic frozen inventory of all JSONL files
// below the Claude and pi roots.
func InventoryTranscripts(ctx context.Context, claudeProjects, piSessions, out string) error {
	if ctx == nil {
		return errors.New("inventory transcripts: context is nil")
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return fmt.Errorf("inventory transcripts context: %w", contextErr)
	}
	claudeRoot, err := absoluteTranscriptRoot(claudeProjects, "claude projects")
	if err != nil {
		return err
	}
	piRoot, err := absoluteTranscriptRoot(piSessions, "pi sessions")
	if err != nil {
		return err
	}
	files := make([]TranscriptInventoryFile, 0)
	for _, candidate := range []struct {
		root string
		kind string
	}{
		{root: claudeRoot, kind: transcriptKindClaude},
		{root: piRoot, kind: transcriptKindPi},
	} {
		found, walkErr := inventoryTranscriptRoot(ctx, candidate.root, candidate.kind)
		if walkErr != nil {
			return walkErr
		}
		files = append(files, found...)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Path != files[j].Path {
			return files[i].Path < files[j].Path
		}
		return files[i].Kind < files[j].Kind
	})
	duplicates := transcriptDuplicates(files)
	inventory := TranscriptInventory{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Roots: TranscriptInventoryRoots{
			ClaudeProjects: claudeRoot,
			PISessions:     piRoot,
		},
		Files:      files,
		Duplicates: duplicates,
	}
	return writeTranscriptInventory(out, inventory)
}

func absoluteTranscriptRoot(root, label string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("inventory transcripts: %s root is blank", label)
	}
	absolute, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("inventory transcripts: resolve %s root %q: %w", label, root, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("inventory transcripts: stat %s root %q: %w", label, absolute, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("inventory transcripts: %s root %q is not a directory", label, absolute)
	}
	return absolute, nil
}

func inventoryTranscriptRoot(ctx context.Context, root, kind string) ([]TranscriptInventoryFile, error) {
	var files []TranscriptInventoryFile
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("inventory transcripts: walk %q: %w", root, walkErr)
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return fmt.Errorf("inventory transcripts: check context: %w", contextErr)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inventory transcripts: stat %q: %w", path, err)
		}
		digest, err := transcriptFileSHA256(path)
		if err != nil {
			return fmt.Errorf("inventory transcripts: hash %q: %w", path, err)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("inventory transcripts: relativize %q: %w", path, err)
		}
		files = append(files, TranscriptInventoryFile{
			Path:    filepath.ToSlash(relative),
			Kind:    kind,
			Size:    info.Size(),
			MTimeNS: info.ModTime().UnixNano(),
			SHA256:  digest,
		})
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			return nil, fmt.Errorf("inventory transcripts: walk %q: %w", root, walkErr)
		}
		return nil, fmt.Errorf("inventory transcripts: walk %q: %w", root, walkErr)
	}
	return files, nil
}

func transcriptFileSHA256(path string) (string, error) {
	file, openErr := openTranscriptFile(path)
	if openErr != nil {
		return "", fmt.Errorf("open transcript for hashing %q: %w", path, openErr)
	}
	defer file.Close()
	hash := sha256.New()
	if _, copyErr := io.Copy(hash, file); copyErr != nil {
		return "", fmt.Errorf("hash transcript %q: %w", path, copyErr)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func transcriptDuplicates(files []TranscriptInventoryFile) []TranscriptInventoryDuplicate {
	byHash := make(map[string][]string)
	for _, file := range files {
		byHash[file.SHA256] = append(byHash[file.SHA256], file.Path)
	}
	duplicates := make([]TranscriptInventoryDuplicate, 0)
	for digest, paths := range byHash {
		if len(paths) < duplicateMinimumFiles {
			continue
		}
		sort.Strings(paths)
		duplicates = append(duplicates, TranscriptInventoryDuplicate{
			SHA256:    digest,
			Canonical: paths[0],
			Others:    append([]string(nil), paths[1:]...),
		})
	}
	sort.Slice(duplicates, func(i, j int) bool {
		return duplicates[i].SHA256 < duplicates[j].SHA256
	})
	return duplicates
}

func writeTranscriptInventory(path string, inventory TranscriptInventory) (result error) {
	if strings.TrimSpace(path) == "" {
		return errors.New("inventory transcripts: output path is blank")
	}
	path = filepath.Clean(path)
	parent := filepath.Dir(path)
	file, createErr := os.CreateTemp(parent, ".patchbay-transcript-inventory-*")
	if createErr != nil {
		return fmt.Errorf("inventory transcripts: create temporary output: %w", createErr)
	}
	temp := file.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := file.Close(); closeErr != nil {
				result = errors.Join(result, fmt.Errorf("inventory transcripts: close temporary output: %w", closeErr))
			}
		}
		if result != nil && temp != "" {
			if removeErr := os.Remove(temp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				result = errors.Join(
					result,
					fmt.Errorf("inventory transcripts: remove temporary output: %w", removeErr),
				)
			}
		}
	}()
	if chmodErr := file.Chmod(transcriptInventoryFileMode); chmodErr != nil {
		return fmt.Errorf("inventory transcripts: secure temporary output: %w", chmodErr)
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	if encodeErr := encoder.Encode(inventory); encodeErr != nil {
		return fmt.Errorf("inventory transcripts: encode output: %w", encodeErr)
	}
	if syncErr := file.Sync(); syncErr != nil {
		return fmt.Errorf("inventory transcripts: sync output: %w", syncErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		return fmt.Errorf("inventory transcripts: close output: %w", closeErr)
	}
	closed = true
	if renameErr := os.Rename(temp, path); renameErr != nil {
		return fmt.Errorf("inventory transcripts: publish output %q: %w", path, renameErr)
	}
	temp = ""
	return nil
}
