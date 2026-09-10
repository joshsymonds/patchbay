package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestInventoryTranscripts(t *testing.T) {
	claude := t.TempDir()
	pi := t.TempDir()
	claudeA := filepath.Join(claude, "proj-a")
	claudeB := filepath.Join(claude, "proj-b")
	if err := os.MkdirAll(filepath.Join(claudeA, "session", "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(claudeB, "session"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pi, "sess"), 0o700); err != nil {
		t.Fatal(err)
	}
	claudeLine := []byte(
		`{"type":"assistant","uuid":"a","timestamp":"2026-09-01T00:00:00Z","requestId":"r","message":{"id":"m","model":"claude-test","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text"}]}}` + "\n",
	)
	subagentLine := []byte(
		`{"type":"assistant","uuid":"sa","timestamp":"2026-09-01T00:00:01Z","requestId":"sr","message":{"model":"claude-test","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text"}]}}` + "\n",
	)
	piLine := []byte(
		`{"type":"message","id":"p","timestamp":"2026-09-01T00:00:02Z","message":{"role":"assistant","model":"pi-test","usage":{"input":1,"output":1},"stopReason":"stop","content":[{"type":"text"}]}}` + "\n",
	)
	for path, contents := range map[string][]byte{
		filepath.Join(claudeA, "session.jsonl"):                         contentsOr(claudeLine),
		filepath.Join(claudeA, "session", "subagents", "agent-x.jsonl"): subagentLine,
		filepath.Join(claudeB, "session.jsonl"):                         claudeLine,
		filepath.Join(pi, "sess", "session.jsonl"):                      piLine,
	} {
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(t.TempDir(), "inventory.json")
	if err := InventoryTranscripts(context.Background(), claude, pi, out); err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var inventory TranscriptInventory
	if err := json.Unmarshal(bytes, &inventory); err != nil {
		t.Fatal(err)
	}
	if inventory.Roots.ClaudeProjects != claude || inventory.Roots.PISessions != pi {
		t.Fatalf("roots = %#v", inventory.Roots)
	}
	gotPaths := make([]string, len(inventory.Files))
	for i, file := range inventory.Files {
		gotPaths[i] = file.Path
	}
	wantPaths := []string{
		"proj-a/session.jsonl",
		"proj-a/session/subagents/agent-x.jsonl",
		"proj-b/session.jsonl",
		"sess/session.jsonl",
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("paths = %#v; want %#v", gotPaths, wantPaths)
	}
	sum := sha256.Sum256(claudeLine)
	if len(inventory.Duplicates) != 1 || inventory.Duplicates[0].SHA256 != hex.EncodeToString(sum[:]) ||
		inventory.Duplicates[0].Canonical != "proj-a/session.jsonl" ||
		!reflect.DeepEqual(inventory.Duplicates[0].Others, []string{"proj-b/session.jsonl"}) {
		t.Fatalf("duplicates = %#v", inventory.Duplicates)
	}
}

func contentsOr(value []byte) []byte { return value }
