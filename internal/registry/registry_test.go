package registry_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/billing"
	"github.com/joshsymonds/patchbay/internal/registry"
)

const validRegistry = `{
  "default_context":"personal",
  "seats":{
    "anthropic":{"upstream":"https://api.anthropic.com","auth_mode":"forward"},
    "luna":{"upstream":"https://openrouter.ai/api","model":"openai/gpt-5.6-luna","auth_mode":"inject","billing":"metered","api_key_env_file":"OPENROUTER_API_KEY_FILE"},
    "sol":{"upstream":"https://openrouter.ai/api","model":"openrouter/sol-1","auth_mode":"inject","billing":"metered","api_key_env_file":"OPENROUTER_API_KEY_FILE"},
    "bedrock":{"auth_mode":"sigv4","aws_profile":"work","aws_region":"us-east-2","model_map_env_file":"BEDROCK_MODEL_MAP_FILE"}
  },
  "contexts":{
    "personal":{"default_seat":"anthropic","models":{"luna-fast":"luna"},"subagents":{"default_seat":"sol","models":{"claude-opus-5":"bedrock"}}},
    "work":{"default_seat":"luna","models":{"openrouter/sol":"sol"}}
  }
}`

func writeRegistry(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	return path
}

func newRegistry(t *testing.T, body string) *registry.Registry {
	t.Helper()
	reg, err := registry.New(writeRegistry(t, filepath.Join(t.TempDir(), "registry.json"), body))
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	return reg
}

func TestNewLoadsRequiredGlobalSeatsAndContexts(t *testing.T) {
	t.Parallel()
	reg := newRegistry(t, validRegistry)

	res, err := reg.Resolve(registry.Selection{}, "luna-fast", nil)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if res.Context != "personal" || res.SeatID != "luna" ||
		res.Seat.Model != "openai/gpt-5.6-luna" {
		t.Fatalf("resolution = %+v", res)
	}
	if res.Match.Kind != registry.SelectionModel || res.Match.Model == nil ||
		*res.Match.Model != "luna-fast" {
		t.Fatalf("match = %+v", res.Match)
	}
}

func TestBareRequestsUseConfiguredDefaultContext(t *testing.T) {
	t.Parallel()
	reg := newRegistry(
		t,
		strings.Replace(
			validRegistry,
			`"default_context":"personal"`,
			`"default_context":"work"`,
			1,
		),
	)
	res, err := reg.Resolve(registry.Selection{}, "unbound", nil)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if res.Context != "work" || res.SeatID != "luna" {
		t.Fatalf("resolution = %+v; want work/luna", res)
	}
}

func TestResolveMarkedSubagentPrecedence(t *testing.T) {
	t.Parallel()
	reg := newRegistry(t, validRegistry)
	marked := &registry.SubagentObservation{AgentID: "agent-1", ParentAgentID: "parent-1"}

	tests := []struct {
		name      string
		model     string
		obs       *registry.SubagentObservation
		wantSeat  string
		wantKind  registry.SelectionKind
		wantModel *string
	}{
		{
			name:      "public binding wins while marked",
			model:     "luna-fast",
			obs:       marked,
			wantSeat:  "luna",
			wantKind:  registry.SelectionModel,
			wantModel: ptr("luna-fast"),
		},
		{
			name:      "exact marked requested model",
			model:     "claude-opus-5",
			obs:       marked,
			wantSeat:  "bedrock",
			wantKind:  registry.SelectionSubagent,
			wantModel: ptr("claude-opus-5"),
		},
		{
			name:     "unnamed marked uses subagent default under the subagent kind",
			model:    "unknown",
			obs:      marked,
			wantSeat: "sol",
			wantKind: registry.SelectionSubagent,
		},
		{
			name:     "unmarked uses context default",
			model:    "unknown",
			wantSeat: "anthropic",
			wantKind: registry.SelectionDefault,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := reg.Resolve(registry.Selection{}, tc.model, tc.obs)
			if err != nil {
				t.Fatalf("Resolve() = %v", err)
			}
			if res.SeatID != tc.wantSeat || res.Match.Kind != tc.wantKind ||
				!reflect.DeepEqual(res.Match.Model, tc.wantModel) {
				t.Fatalf("resolution = %+v", res)
			}
			if !reflect.DeepEqual(res.Subagent, tc.obs) {
				t.Fatalf("subagent = %+v; want %+v", res.Subagent, tc.obs)
			}
		})
	}
}

func TestContextWithoutSubagentsUsesOrdinaryPolicyForMarkedTraffic(t *testing.T) {
	t.Parallel()
	reg := newRegistry(t, validRegistry)
	res, err := reg.Resolve(
		registry.Selection{Name: "work", Explicit: true},
		"unknown",
		&registry.SubagentObservation{AgentID: "a"},
	)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if res.SeatID != "luna" || res.Match.Kind != registry.SelectionDefault {
		t.Fatalf("resolution = %+v", res)
	}
}

func TestForeignRewriteTargetGuardUsesPublicBindingsOnly(t *testing.T) {
	t.Parallel()
	body := `{"default_context":"personal","seats":{` +
		`"default":{"upstream":"https://api.anthropic.com","auth_mode":"forward"},` +
		`"foreign-a":{"upstream":"https://one.example","model":"target","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY"},` +
		`"foreign-b":{"upstream":"https://two.example","model":"target","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY"},` +
		`"same":{"upstream":"https://api.anthropic.com","model":"target","auth_mode":"forward"},` +
		`"sub":{"upstream":"https://three.example","model":"sub-target","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY"}` +
		`},"contexts":{"personal":{"default_seat":"default","models":{"z":"foreign-b","a":"foreign-a"},"subagents":{"default_seat":"sub","models":{}}}}}`
	reg := newRegistry(t, body)

	_, err := reg.Resolve(registry.Selection{}, "target", nil)
	if !errors.Is(err, registry.ErrRewriteTarget) || !strings.Contains(err.Error(), `"a"`) {
		t.Fatalf("Resolve(target) = %v; want lexical public selector a", err)
	}
	if _, err := reg.Resolve(registry.Selection{}, "sub-target", nil); err != nil {
		t.Fatalf("subagent-only target was guarded: %v", err)
	}

	body = strings.Replace(
		body,
		`"z":"foreign-b","a":"foreign-a"`,
		`"z":"foreign-b","a":"foreign-a","same":"same"`,
		1,
	)
	reg = newRegistry(t, body)
	if _, err := reg.Resolve(registry.Selection{}, "target", nil); err != nil {
		t.Fatalf("default-upstream public binding did not exempt target: %v", err)
	}
}

func TestInvalidHotReloadInstallsSentinelAndRecovers(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, validRegistry)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, base, base); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.New(path)
	if err != nil {
		t.Fatal(err)
	}

	writeRegistry(t, path, `{}`)
	if err := os.Chtimes(path, base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Resolve(
		registry.Selection{},
		"luna-fast",
		nil,
	); !errors.Is(
		err,
		registry.ErrInvalid,
	) {
		t.Fatalf("Resolve after invalid replacement = %v; want ErrInvalid", err)
	}
	state := reg.State()
	if state.Valid || state.DefaultContext != "" || len(state.Seats) != 0 ||
		len(state.Contexts) != 0 ||
		state.Error == "" {
		t.Fatalf("invalid state = %+v", state)
	}

	writeRegistry(t, path, validRegistry)
	if err := os.Chtimes(path, base.Add(2*time.Hour), base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Resolve(registry.Selection{}, "luna-fast", nil); err != nil {
		t.Fatalf("Resolve after repair = %v", err)
	}
}

// rejectedValue is a distinctive string an invalid registry names in a field
// validation quotes back. It stands in for anything an operator might have
// pasted into the wrong field — a live key, a path, a host — so a test can tell
// a surface that keeps the detail from one that must not.
const rejectedValue = "sk-live-secret"

// secretRegistry is an invalid document whose validation error quotes
// rejectedValue: the env-file field names a value that is not an environment
// variable name at all.
const secretRegistry = `{
  "default_context":"personal",
  "seats":{"luna":{"upstream":"https://openrouter.ai/api","model":"openai/gpt-5.6-luna","auth_mode":"inject","billing":"metered","api_key_env_file":"` + rejectedValue + `"}},
  "contexts":{"personal":{"default_seat":"luna","models":{"luna-fast":"luna"}}}
}`

// TestInvalidRuntimeSurfacesSanitizeWhatOperatorSurfacesKeep splits the two
// audiences of a hot-reload failure. An operator asking the registry what is
// wrong needs the field and the value to fix the file; every surface a request
// can reach — the resolver, the introspected snapshot's error, and the resolver
// run against that snapshot — answers an unauthenticated caller and must say
// only that the registry is invalid, never what the rejected file contains.
func TestInvalidRuntimeSurfacesSanitizeWhatOperatorSurfacesKeep(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, validRegistry)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	chtimes(t, path, base)
	reg, err := registry.New(path)
	if err != nil {
		t.Fatal(err)
	}

	writeRegistry(t, path, secretRegistry)
	chtimes(t, path, base.Add(time.Hour))

	operator := reg.Err()
	if !errors.Is(operator, registry.ErrInvalid) {
		t.Fatalf("Err() = %v; want ErrInvalid", operator)
	}
	for _, detail := range []string{rejectedValue, "api_key_env_file", path} {
		if !strings.Contains(operator.Error(), detail) {
			t.Errorf("Err() = %q; want it to keep the operator detail %q", operator, detail)
		}
	}

	_, resolveErr := reg.Resolve(registry.Selection{}, "luna-fast", nil)
	if !errors.Is(resolveErr, registry.ErrInvalid) {
		t.Fatalf("Resolve() = %v; want ErrInvalid", resolveErr)
	}
	assertSanitized(t, "Resolve()", resolveErr.Error(), path)

	state := reg.State()
	if state.Valid || state.Error == "" {
		t.Fatalf("invalid State() = %+v; want an invalid snapshot naming its own sentinel", state)
	}
	assertSanitized(t, "State().Error", state.Error, path)

	_, stateErr := registry.ResolveState(state, registry.Selection{}, "luna-fast", nil)
	if !errors.Is(stateErr, registry.ErrInvalid) {
		t.Fatalf("ResolveState() = %v; want ErrInvalid", stateErr)
	}
	assertSanitized(t, "ResolveState()", stateErr.Error(), path)
}

// assertSanitized fails when a runtime-safe message carries anything of the
// rejected file: the value it quoted or the path it was read from.
func assertSanitized(t *testing.T, surface, message, path string) {
	t.Helper()
	if strings.Contains(message, rejectedValue) {
		t.Errorf("%s = %q; want the rejected value withheld", surface, message)
	}
	if strings.Contains(message, path) {
		t.Errorf("%s = %q; want the registry path withheld", surface, message)
	}
}

// TestObservedAcquisitionsCarrySnapshotGenerationsAndPairedDiagnostics pins the
// generation contract: repeated observations of one immutable snapshot share a
// generation and diagnostic, while installing a changed file advances it.
func TestObservedAcquisitionsCarrySnapshotGenerationsAndPairedDiagnostics(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, validRegistry)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	chtimes(t, path, base)
	reg, err := registry.New(path)
	if err != nil {
		t.Fatal(err)
	}

	_, first, resolveErr := reg.ResolveObserved(registry.Selection{}, "luna-fast", nil)
	if resolveErr != nil || first.Err != nil {
		t.Fatalf("ResolveObserved() = %v, observation %+v; want valid", resolveErr, first)
	}
	_, repeated, resolveErr := reg.ResolveObserved(registry.Selection{}, "luna-fast", nil)
	if resolveErr != nil || repeated.Err != nil {
		t.Fatalf("repeated ResolveObserved() = %v, observation %+v; want valid", resolveErr, repeated)
	}
	if first.Sequence == 0 || repeated.Sequence != first.Sequence {
		t.Fatalf(
			"unchanged snapshot generations = %d, %d; want one nonzero generation",
			first.Sequence,
			repeated.Sequence,
		)
	}

	writeRegistry(t, path, secretRegistry)
	chtimes(t, path, base.Add(time.Hour))
	state, second := reg.InspectObserved()
	if second.Sequence <= first.Sequence {
		t.Fatalf("snapshot generation did not advance: first=%d second=%d", first.Sequence, second.Sequence)
	}
	if state.Valid || !errors.Is(second.Err, registry.ErrInvalid) {
		t.Fatalf("InspectObserved() = %+v, %+v; want paired invalid state and detail", state, second)
	}
	_, repeatedInvalid := reg.InspectObserved()
	if repeatedInvalid.Sequence != second.Sequence || repeatedInvalid.Err.Error() != second.Err.Error() {
		t.Fatalf(
			"unchanged invalid observations = %+v, %+v; want one generation and diagnostic",
			second,
			repeatedInvalid,
		)
	}
	for _, detail := range []string{rejectedValue, "api_key_env_file", path} {
		if !strings.Contains(second.Err.Error(), detail) {
			t.Errorf("InspectObserved() error = %q; want detail %q", second.Err, detail)
		}
	}
}

// TestInspectPairsOneSnapshotWithItsOperatorError pins what an operator surface
// needs and State and Err cannot give it separately: both halves of one
// snapshot. Asked one at a time, a registry replaced between the two calls
// answers each from a different version of the file — a summary of the document
// that opened beside a refusal from the one that replaced it, or counts printed
// for a document nothing validated. Inspect refreshes once and pairs the
// sanitized snapshot an operator is shown with the detailed error from the same
// immutable document generation.
func TestInspectPairsOneSnapshotWithItsOperatorError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, validRegistry)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	chtimes(t, path, base)
	reg, err := registry.New(path)
	if err != nil {
		t.Fatal(err)
	}

	state, inspectErr := reg.Inspect()
	if inspectErr != nil {
		t.Fatalf("Inspect() = %v; want a valid registry inspected without error", inspectErr)
	}
	if !state.Valid || state.DefaultContext != "personal" || len(state.Seats) != 4 ||
		len(state.Contexts) != 2 {
		t.Fatalf("valid Inspect() state = %+v; want the whole current document", state)
	}

	writeRegistry(t, path, secretRegistry)
	chtimes(t, path, base.Add(time.Hour))

	state, inspectErr = reg.Inspect()
	if !errors.Is(inspectErr, registry.ErrInvalid) {
		t.Fatalf("Inspect() after invalid replacement = %v; want ErrInvalid", inspectErr)
	}
	for _, detail := range []string{rejectedValue, "api_key_env_file", path} {
		if !strings.Contains(inspectErr.Error(), detail) {
			t.Errorf("Inspect() = %q; want it to keep the operator detail %q", inspectErr, detail)
		}
	}
	if state.Valid || state.Error == "" || state.DefaultContext != "" ||
		len(state.Seats) != 0 || len(state.Contexts) != 0 {
		t.Fatalf("invalid Inspect() state = %+v; want the sanitized empty snapshot", state)
	}
	assertSanitized(t, "Inspect() state.Error", state.Error, path)
}

func TestInFlightContextResolutionSurvivesInvalidReload(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, validRegistry)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, base, base); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.New(path)
	if err != nil {
		t.Fatal(err)
	}
	ctxRes, err := reg.Resolve(registry.Selection{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	writeRegistry(t, path, `{}`)
	if err := os.Chtimes(path, base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Resolve(registry.Selection{}, "", nil); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("Resolve = %v", err)
	}

	res, err := ctxRes.Select("luna-fast", nil)
	if err != nil || res.SeatID != "luna" {
		t.Fatalf("cached Select = %+v, %v", res, err)
	}
}

func TestNewRejectsMissingUnreadableUnparsableAndInvalidRegistry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		makePath func(*testing.T) string
	}{
		{name: "missing", makePath: func(t *testing.T) string {
			t.Helper()
			return filepath.Join(t.TempDir(), "missing.json")
		}},
		{name: "unreadable", makePath: func(t *testing.T) string {
			t.Helper()
			p := filepath.Join(t.TempDir(), "registry.json")
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{name: "unparsable", makePath: func(t *testing.T) string {
			t.Helper()
			return writeRegistry(t, filepath.Join(t.TempDir(), "registry.json"), `{`)
		}},
		{name: "invalid", makePath: func(t *testing.T) string {
			t.Helper()
			return writeRegistry(t, filepath.Join(t.TempDir(), "registry.json"), `{}`)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := registry.New(tc.makePath(t)); err == nil {
				t.Fatal("New() = nil error")
			}
		})
	}
}

func TestUnknownExplicitContextStaysNotFound(t *testing.T) {
	t.Parallel()
	reg := newRegistry(t, validRegistry)
	_, err := reg.Resolve(registry.Selection{Name: "missing", Explicit: true}, "x", nil)
	if !errors.Is(err, registry.ErrUnknownContext) {
		t.Fatalf("Resolve = %v", err)
	}
}

func ptr(value string) *string { return &value }

// chtimes pins a file's modification time, so a reload test can control the one
// stat field a same-size rewrite would otherwise change on its own.
func chtimes(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// pinnedRegistry is a one-Seat document whose only variable is the model that
// Seat rewrites to. Both spellings are the same length, so file size can never
// explain a reload.
func pinnedRegistry(model string) string {
	return `{"default_context":"personal","seats":{"pin":{"upstream":"https://api.anthropic.com",` +
		`"model":"` + model + `","auth_mode":"forward"}},` +
		`"contexts":{"personal":{"default_seat":"pin","models":{}}}}`
}

func pinnedModel(t *testing.T, reg *registry.Registry) string {
	t.Helper()
	res, err := reg.Resolve(registry.Selection{}, "anything", nil)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	return res.Seat.Model
}

// TestReloadOnModTimeChange pins the ordinary in-place rewrite: same path, same
// size, later modification time.
func TestReloadOnModTimeChange(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, pinnedRegistry("model-one"))
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	chtimes(t, path, base)

	reg, err := registry.New(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := pinnedModel(t, reg); got != "model-one" {
		t.Fatalf("first Resolve model = %q; want %q", got, "model-one")
	}

	writeRegistry(t, path, pinnedRegistry("model-two"))
	chtimes(t, path, base.Add(time.Hour))

	if got := pinnedModel(t, reg); got != "model-two" {
		t.Fatalf("Resolve after mtime change = %q; want the reloaded %q", got, "model-two")
	}
}

// TestResolveUsesTheCacheWhenNothingChanged pins the other half: a stat that has
// not moved must not cost a re-read on every request.
func TestResolveUsesTheCacheWhenNothingChanged(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, pinnedRegistry("model-one"))
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	chtimes(t, path, base)

	reg, err := registry.New(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := pinnedModel(t, reg); got != "model-one" {
		t.Fatalf("first Resolve model = %q; want %q", got, "model-one")
	}

	// Same size, same mtime, different content: the cached parse must stand.
	writeRegistry(t, path, pinnedRegistry("model-two"))
	chtimes(t, path, base)

	if got := pinnedModel(t, reg); got != "model-one" {
		t.Fatalf("Resolve with an unchanged stat = %q; want the cached %q", got, "model-one")
	}
}

// TestReloadOnSymlinkRetarget pins the home-manager case: the path never
// changes, the file it resolves to does, and store timestamps are normalised so
// the modification time cannot tell the two generations apart.
func TestReloadOnSymlinkRetarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	one := writeRegistry(t, filepath.Join(dir, "one.json"), pinnedRegistry("model-one"))
	two := writeRegistry(t, filepath.Join(dir, "two.json"), pinnedRegistry("model-two"))
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	chtimes(t, one, base)
	chtimes(t, two, base)

	link := filepath.Join(dir, "current.json")
	if err := os.Symlink(one, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, one, err)
	}

	reg, err := registry.New(link)
	if err != nil {
		t.Fatal(err)
	}
	if got := pinnedModel(t, reg); got != "model-one" {
		t.Fatalf("first Resolve model = %q; want %q", got, "model-one")
	}

	if removeErr := os.Remove(link); removeErr != nil {
		t.Fatalf("remove symlink: %v", removeErr)
	}
	if linkErr := os.Symlink(two, link); linkErr != nil {
		t.Fatalf("retarget symlink %s -> %s: %v", link, two, linkErr)
	}

	if got := pinnedModel(t, reg); got != "model-two" {
		t.Fatalf("Resolve after symlink retarget = %q; want the reloaded %q", got, "model-two")
	}
}

// TestHotMissingOrUnreadableRegistryFailsClosed pins the two ways a registry can
// stop being readable at runtime. Neither one may serve the last good document:
// there is no Seat outside the registry, so the only honest answer is refusal.
func TestHotMissingOrUnreadableRegistryFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		damage func(*testing.T, string)
	}{
		{
			name: "removed",
			damage: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove registry: %v", err)
				}
			},
		},
		{
			name: "replaced by a directory",
			damage: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove registry: %v", err)
				}
				if err := os.Mkdir(path, 0o750); err != nil {
					t.Fatalf("mkdir %s: %v", path, err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "registry.json")
			writeRegistry(t, path, validRegistry)
			reg, err := registry.New(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, resolveErr := reg.Resolve(registry.Selection{}, "luna-fast", nil); resolveErr != nil {
				t.Fatalf("Resolve before the break = %v", resolveErr)
			}

			tc.damage(t, path)

			if _, resolveErr := reg.Resolve(
				registry.Selection{},
				"luna-fast",
				nil,
			); !errors.Is(resolveErr, registry.ErrInvalid) {
				t.Fatalf("Resolve = %v; want ErrInvalid", resolveErr)
			}
			if !errors.Is(reg.Err(), registry.ErrInvalid) {
				t.Fatalf("Err() = %v; want ErrInvalid", reg.Err())
			}
			if state := reg.State(); state.Valid || len(state.Seats) != 0 || len(state.Contexts) != 0 {
				t.Fatalf("state after the break = %+v; want no stale summaries", state)
			}
		})
	}
}

// TestConcurrentResolveIsSafe runs the resolver the way the gateway does: many
// requests at once against one shared registry, under -race.
func TestConcurrentResolveIsSafe(t *testing.T) {
	t.Parallel()
	const (
		workers = 8
		rounds  = 50
	)
	reg := newRegistry(t, validRegistry)

	var group sync.WaitGroup
	start := make(chan struct{})
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			for range rounds {
				res, err := reg.Resolve(registry.Selection{}, "luna-fast", nil)
				if err != nil || res.SeatID != "luna" {
					t.Errorf("public Resolve = %+v, %v", res, err)
					return
				}
				marked, markedErr := reg.Resolve(
					registry.Selection{},
					"claude-opus-5",
					&registry.SubagentObservation{AgentID: "agent"},
				)
				if markedErr != nil || marked.SeatID != "bedrock" {
					t.Errorf("marked Resolve = %+v, %v", marked, markedErr)
					return
				}
				work, workErr := reg.Resolve(
					registry.Selection{Name: "work", Explicit: true},
					"unbound",
					nil,
				)
				if workErr != nil || work.SeatID != "luna" {
					t.Errorf("work Resolve = %+v, %v", work, workErr)
					return
				}
				if err := reg.Err(); err != nil {
					t.Errorf("Err() = %v; want nil for a valid registry", err)
					return
				}
			}
		}()
	}
	close(start)
	group.Wait()
}

// TestStateAndResolutionHandOutIsolatedCopies pins the boundary around the
// registry's own maps. Both are handed to callers that legitimately mutate what
// they get — the CLI builds a State into a resolver, the pipeline merges a
// Seat's extra_body — and a shared backing map would let one request rewrite the
// document every later request reads.
func TestStateAndResolutionHandOutIsolatedCopies(t *testing.T) {
	t.Parallel()
	body := strings.Replace(
		validRegistry,
		`"luna":{"upstream"`,
		`"luna":{"extra_body":{"reasoning_effort":"low"},"upstream"`,
		1,
	)
	reg := newRegistry(t, body)

	state := reg.State()
	state.Seats["luna"] = registry.Seat{Model: "tampered"}
	delete(state.Contexts, "personal")
	if again := reg.State(); again.Seats["luna"].Model != "openai/gpt-5.6-luna" ||
		len(again.Contexts) != 2 {
		t.Fatalf("State() was mutated through a previous caller's copy: %+v", again)
	}

	res, err := reg.Resolve(registry.Selection{}, "luna-fast", nil)
	if err != nil {
		t.Fatal(err)
	}

	models := res.Models()
	models["luna-fast"] = "tampered"
	models["injected"] = "luna"
	if again := res.Models(); again["luna-fast"] != "luna" || len(again) != 1 {
		t.Fatalf("Models() shares its backing map: %+v", again)
	}

	policy := res.SubagentPolicy()
	policy.DefaultSeat = "tampered"
	policy.Models["claude-opus-5"] = "tampered"
	if again := res.SubagentPolicy(); again.DefaultSeat != "sol" ||
		again.Models["claude-opus-5"] != "bedrock" {
		t.Fatalf("SubagentPolicy() shares its backing values: %+v", again)
	}

	bindings := res.PublicBindings()
	if len(bindings) != 1 {
		t.Fatalf("PublicBindings() = %+v", bindings)
	}
	if len(bindings[0].Seat.ExtraBody) == 0 {
		t.Fatal("the fixture Seat has no extra_body to protect")
	}
	bindings[0].Seat.ExtraBody[0] = 'X'
	if again := res.PublicBindings(); again[0].Seat.ExtraBody[0] != '{' {
		t.Fatalf("PublicBindings() shares its extra_body bytes: %s", again[0].Seat.ExtraBody)
	}

	_, seat := res.DefaultSeat()
	if len(seat.ExtraBody) != 0 {
		t.Fatalf("default Seat has unexpected extra_body: %s", seat.ExtraBody)
	}
}

// TestResolveStateSnapshotsTheStateItWasGiven closes the boundary on the one
// resolver whose whole input is caller-owned. A State is an exported struct of
// exported maps, slices and pointers handed to ResolveState by value — which
// copies the struct and nothing it points at. The Resolution that comes back is
// documented as an immutable snapshot and is read long after the call: Select
// re-runs the precedence, Models, SubagentPolicy, DefaultSeat and
// PublicBindings hand out copies of it. Every one of those reads the caller's
// own maps unless the snapshot is taken at the door.
func TestResolveStateSnapshotsTheStateItWasGiven(t *testing.T) {
	t.Parallel()
	const wantExtraBody = `{"reasoning_effort":"low"}`
	reg := newRegistry(t, strings.Replace(
		validRegistry,
		`"luna":{"upstream"`,
		`"luna":{"extra_body":`+wantExtraBody+`,"upstream"`,
		1,
	))

	state := reg.State()
	res, err := registry.ResolveState(
		state,
		registry.Selection{Name: "personal", Explicit: true},
		"luna-fast",
		nil,
	)
	if err != nil {
		t.Fatalf("ResolveState() = %v", err)
	}
	if res.SeatID != "luna" || string(res.Seat.ExtraBody) != wantExtraBody {
		t.Fatalf("resolution = %+v; want the luna Seat with its extra_body", res)
	}

	// Everything the caller still holds a reference into, rewritten: the Seat
	// map and one Seat's extra_body bytes, the Context's public bindings, its
	// marked-subagent policy, and the Context map itself.
	luna := state.Seats["luna"]
	luna.ExtraBody[0] = 'X'
	for _, id := range []string{"anthropic", "luna", "sol", "bedrock"} {
		state.Seats[id] = registry.Seat{Model: "tampered", Upstream: "https://tampered.example"}
	}
	personal := state.Contexts["personal"]
	personal.Models["luna-fast"] = "anthropic"
	personal.Models["injected"] = "anthropic"
	personal.Subagents.DefaultSeat = "anthropic"
	personal.Subagents.Models["claude-opus-5"] = "anthropic"
	delete(state.Contexts, "personal")
	state.DefaultContext = "tampered"

	if res.SeatID != "luna" || res.Seat.Model != "openai/gpt-5.6-luna" ||
		string(res.Seat.ExtraBody) != wantExtraBody {
		t.Errorf("selected Seat = %+v; want the pre-mutation snapshot", res.Seat)
	}

	again, selectErr := res.Select("luna-fast", nil)
	if selectErr != nil || again.SeatID != "luna" ||
		string(again.Seat.ExtraBody) != wantExtraBody {
		t.Errorf("a later Select() = %+v, %v; want the pre-mutation snapshot", again, selectErr)
	}

	marked, markedErr := res.Select(
		"claude-opus-5",
		&registry.SubagentObservation{AgentID: "agent-1"},
	)
	if markedErr != nil || marked.SeatID != "bedrock" ||
		marked.Match.Kind != registry.SelectionSubagent {
		t.Errorf("a later marked Select() = %+v, %v; want the bedrock binding", marked, markedErr)
	}

	fallback, fallbackErr := res.Select("unbound", nil)
	if fallbackErr != nil || fallback.SeatID != "anthropic" ||
		fallback.Seat.Upstream != "https://api.anthropic.com" {
		t.Errorf("a later default Select() = %+v, %v; want the anthropic Seat", fallback, fallbackErr)
	}

	if models := res.Models(); len(models) != 1 || models["luna-fast"] != "luna" {
		t.Errorf("Models() = %+v; want the pre-mutation bindings", models)
	}

	policy := res.SubagentPolicy()
	if policy == nil || policy.DefaultSeat != "sol" || policy.Models["claude-opus-5"] != "bedrock" {
		t.Errorf("SubagentPolicy() = %+v; want the pre-mutation policy", policy)
	}

	seatID, seat := res.DefaultSeat()
	if seatID != "anthropic" || seat.Upstream != "https://api.anthropic.com" || seat.Model != "" {
		t.Errorf("DefaultSeat() = %q, %+v; want the pre-mutation anthropic Seat", seatID, seat)
	}

	bindings := res.PublicBindings()
	if len(bindings) != 1 || bindings[0].Selector != "luna-fast" || bindings[0].SeatID != "luna" ||
		string(bindings[0].Seat.ExtraBody) != wantExtraBody {
		t.Errorf("PublicBindings() = %+v; want the pre-mutation binding", bindings)
	}
}

// TestSelectedSeatExtraBodyIsIsolated closes the last hole in that boundary. A
// Resolution is documented as an immutable snapshot, but the Seat it selects is
// handed out whole and the pipeline merges the caller's own body over its
// extra_body — so a shared slice would let one request rewrite what every later
// request, and the registry document itself, reads.
func TestSelectedSeatExtraBodyIsIsolated(t *testing.T) {
	t.Parallel()
	const wantExtraBody = `{"reasoning_effort":"low"}`
	reg := newRegistry(t, strings.Replace(
		validRegistry,
		`"luna":{"upstream"`,
		`"luna":{"extra_body":`+wantExtraBody+`,"upstream"`,
		1,
	))

	ctxRes, err := reg.Resolve(registry.Selection{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ctxRes.Select("luna-fast", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Seat.ExtraBody) != wantExtraBody {
		t.Fatalf("selected Seat extra_body = %s; want the fixture's %s", res.Seat.ExtraBody, wantExtraBody)
	}

	res.Seat.ExtraBody[0] = 'X'

	again, err := ctxRes.Select("luna-fast", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(again.Seat.ExtraBody) != wantExtraBody {
		t.Errorf("a later Select() sees %s; want %s", again.Seat.ExtraBody, wantExtraBody)
	}

	fresh, err := reg.Resolve(registry.Selection{}, "luna-fast", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(fresh.Seat.ExtraBody) != wantExtraBody {
		t.Errorf("a fresh Resolve() sees %s; want %s", fresh.Seat.ExtraBody, wantExtraBody)
	}
}

// TestSelectedSeatCarriesItsBillingDeclaration follows the declared class all
// the way to the resolved Seat. Accounting reads it from there, so a class that
// parsed but did not survive selection would be an unattributed charge.
func TestSelectedSeatCarriesItsBillingDeclaration(t *testing.T) {
	t.Parallel()
	const injectSeat = `{"upstream":"https://openrouter.ai/api","model":"gpt",` +
		`"auth_mode":"inject","api_key_env_file":"KEY_FILE"`
	reg := newRegistry(t, `{"default_context":"personal","seats":{`+
		`"anthropic":{"upstream":"https://api.anthropic.com","auth_mode":"forward"},`+
		`"subscription":`+injectSeat+`,"billing":"subscription"},`+
		`"metered":`+injectSeat+`,"billing":"metered"},`+
		`"self-hosted":`+injectSeat+`,"billing":"self_hosted"}},`+
		`"contexts":{"personal":{"default_seat":"anthropic","models":{`+
		`"sub":"subscription","met":"metered","own":"self-hosted"}}}}`)

	tests := []struct {
		selector string
		want     billing.Class
		policy   billing.Policy
	}{
		{selector: "sub", want: billing.ClassSubscription, policy: billing.PolicyExplicit},
		{selector: "met", want: billing.ClassMetered, policy: billing.PolicyExplicit},
		{selector: "own", want: billing.ClassSelfHosted, policy: billing.PolicyExplicit},
	}
	for _, tc := range tests {
		t.Run(tc.selector, func(t *testing.T) {
			t.Parallel()
			res, err := reg.Resolve(registry.Selection{}, tc.selector, nil)
			if err != nil {
				t.Fatalf("Resolve(%q) = %v", tc.selector, err)
			}
			if res.Seat.BillingClass != tc.want || !res.Seat.BillingClassSet {
				t.Fatalf("Resolve(%q) billing = %q, present %v; want %q, present true",
					tc.selector, res.Seat.BillingClass, res.Seat.BillingClassSet, tc.want)
			}
			if got := res.Seat.BillingPolicy(); got != tc.policy {
				t.Fatalf("Resolve(%q) BillingPolicy() = %q; want %q", tc.selector, got, tc.policy)
			}
		})
	}

	// The forward Seat every unbound model falls through to declares nothing:
	// the caller's own credential is what classifies it.
	fallthroughRes, err := reg.Resolve(registry.Selection{}, "claude-sonnet-5", nil)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if fallthroughRes.Seat.BillingClassSet || fallthroughRes.Seat.BillingClass != "" {
		t.Fatalf("default Seat billing = %q, present %v; want no declaration",
			fallthroughRes.Seat.BillingClass, fallthroughRes.Seat.BillingClassSet)
	}
	if got := fallthroughRes.Seat.BillingPolicy(); got != billing.PolicyCallerDerived {
		t.Fatalf("default Seat BillingPolicy() = %q; want %q", got, billing.PolicyCallerDerived)
	}
}

// TestSeatBillingPolicyDependsOnlyOnAuthMode pins the policy to the auth mode
// alone, because the auth mode is what decides whose credential is spent. A
// declared class narrows an explicit policy; it never changes which policy the
// Seat has.
func TestSeatBillingPolicyDependsOnlyOnAuthMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		seat registry.Seat
		want billing.Policy
	}{
		{
			name: "forward credentials classify themselves",
			seat: registry.Seat{AuthMode: registry.AuthForward},
			want: billing.PolicyCallerDerived,
		},
		{
			name: "sigv4 is metered by the signed account",
			seat: registry.Seat{AuthMode: registry.AuthSigV4},
			want: billing.PolicySigV4Metered,
		},
		{
			name: "inject declares its class",
			seat: registry.Seat{
				AuthMode:        registry.AuthInject,
				BillingClass:    billing.ClassSubscription,
				BillingClassSet: true,
			},
			want: billing.PolicyExplicit,
		},
		{
			name: "inject has an explicit policy before the class is read",
			seat: registry.Seat{AuthMode: registry.AuthInject},
			want: billing.PolicyExplicit,
		},
		{name: "an unset auth mode has no policy", seat: registry.Seat{}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.seat.BillingPolicy(); got != tc.want {
				t.Fatalf("BillingPolicy() = %q; want %q", got, tc.want)
			}
		})
	}
}
