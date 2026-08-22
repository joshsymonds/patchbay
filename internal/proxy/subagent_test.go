package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/registry"
	usagepkg "github.com/joshsymonds/patchbay/internal/usage"
)

// forwardDeadline bounds waiting for a request the local stub upstream should
// already have received. It is a hang guard rather than a timing assumption: a
// request the pipeline refused before spending anything never arrives at all,
// and that has to fail the test rather than stall the suite behind it.
const forwardDeadline = 10 * time.Second

// countingAccountant satisfies the required Accountant without asserting
// anything: these tests are about which Seat marked traffic selects, and the
// durable-store assertions live in accounting_test.go.
type countingAccountant struct{}

func (countingAccountant) InsertPending(context.Context, usagepkg.PendingRequest) (int64, error) {
	return 1, nil
}

func (countingAccountant) MarkUpstreamDispatched(context.Context, int64, time.Time, string) error {
	return nil
}

func (countingAccountant) Finalize(context.Context, int64, usagepkg.FinalizedRequest) error {
	return nil
}

// newSubagentProxy builds a pipeline for one subagent case, failing the test if
// the pipeline refuses its own configuration.
func newSubagentProxy(t *testing.T, reg *registry.Registry) *Proxy {
	t.Helper()

	pipeline, err := New(Config{Registry: reg, Accountant: countingAccountant{}})
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}

	return pipeline
}

func TestObserveSubagentRequiresOneExactPrintableAgentID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		values []string
		marked bool
		want   string
	}{
		{name: "one printable value", values: []string{"agent/One:2"}, marked: true, want: "agent/One:2"},
		{name: "empty", values: []string{""}},
		{name: "duplicate", values: []string{"one", "two"}},
		{name: "oversized", values: []string{strings.Repeat("a", 257)}},
		{name: "space", values: []string{"agent one"}},
		{name: "control", values: []string{"agent\n"}},
		{name: "non ascii", values: []string{"agé"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			header := http.Header{}
			for _, value := range tc.values {
				header.Add(ClaudeCodeAgentIDHeader, value)
			}
			obs := observeSubagent(header)
			if (obs != nil) != tc.marked {
				t.Fatalf("observeSubagent() = %+v, marked=%v", obs, tc.marked)
			}
			if obs != nil && obs.AgentID != tc.want {
				t.Fatalf("AgentID = %q; want %q", obs.AgentID, tc.want)
			}
		})
	}
}

func TestObserveSubagentParentNeverMarksAndIsRecordedOnlyWhenValid(t *testing.T) {
	t.Parallel()
	parentOnly := http.Header{ClaudeCodeParentAgentIDHeader: []string{"parent"}}
	if obs := observeSubagent(parentOnly); obs != nil {
		t.Fatalf("parent-only observation = %+v", obs)
	}

	tests := []struct {
		name    string
		parents []string
		want    string
	}{
		{name: "valid", parents: []string{"parent/One"}, want: "parent/One"},
		{name: "empty", parents: []string{""}},
		{name: "duplicate", parents: []string{"one", "two"}},
		{name: "invalid bytes", parents: []string{"parent id"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			header := http.Header{ClaudeCodeAgentIDHeader: []string{"agent"}}
			for _, value := range tc.parents {
				header.Add(ClaudeCodeParentAgentIDHeader, value)
			}
			obs := observeSubagent(header)
			if obs == nil || obs.AgentID != "agent" || obs.ParentAgentID != tc.want {
				t.Fatalf("observation = %+v; parent=%q", obs, tc.want)
			}
		})
	}
}

func TestMarkedSubagentHeadersAreForwardedUnchanged(t *testing.T) {
	const keyEnv = "PATCHBAY_SUBAGENT_TEST_KEY_FILE"
	writeSubagentTestFile(t, keyEnv, "provider-key")
	writeSubagentTestFile(t, "PATCHBAY_CALLER_KEY_FILE", "caller-key")
	headers := make(chan http.Header, 1)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(stub.Close)
	body := []byte(fmt.Sprintf(`{"default_context":"personal","seats":{`+
		`"anthropic":{"upstream":"https://api.anthropic.com","auth_mode":"forward"},`+
		`"worker":{"upstream":%q,"model":"vendor/worker","auth_mode":"inject",`+
		`"api_key_env_file":%q,"billing":"metered"}},`+
		`"contexts":{"personal":{"default_seat":"anthropic","models":{},`+
		`"subagents":{"default_seat":"worker","models":{}}}}}`, stub.URL, keyEnv))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5"}`))
	req.Header.Set(CallerKeyHeader, "caller-key")
	req.Header[ClaudeCodeAgentIDHeader] = []string{"agent/One:2"}
	req.Header[ClaudeCodeParentAgentIDHeader] = []string{"parent/One:1"}
	rec := httptest.NewRecorder()
	newSubagentProxy(t, newSubagentTestRegistry(t, body)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	select {
	case call := <-headers:
		if got := call.Values(ClaudeCodeAgentIDHeader); len(got) != 1 || got[0] != "agent/One:2" {
			t.Fatalf("agent header=%q", got)
		}
		if got := call.Values(ClaudeCodeParentAgentIDHeader); len(got) != 1 || got[0] != "parent/One:1" {
			t.Fatalf("parent header=%q", got)
		}
	case <-time.After(forwardDeadline):
		t.Fatal("no request reached the subagent Seat's upstream; no forwarded headers to check")
	}
}

func TestMarkedSubagentSeatsStillRequireCallerKeyBeforeCredentialSpend(t *testing.T) {
	writeSubagentTestFile(t, "PATCHBAY_CALLER_KEY_FILE", "caller-key")
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(stub.Close)
	tests := []struct {
		name string
		seat string
	}{
		{
			name: "inject",
			seat: fmt.Sprintf(
				`{"upstream":%q,"model":"vendor/worker","auth_mode":"inject",`+
					`"api_key_env_file":"UNREADABLE_KEY","billing":"metered"}`,
				stub.URL,
			),
		},
		{
			name: "sigv4",
			seat: `{"model":"claude-sonnet-5","auth_mode":"sigv4","aws_profile":"missing",` +
				`"aws_region":"us-east-1","model_map_env_file":"UNREADABLE_MAP"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"default_context":"personal","seats":{` +
				`"anthropic":{"upstream":"https://api.anthropic.com","auth_mode":"forward"},` +
				`"worker":` + tc.seat + `},"contexts":{"personal":{"default_seat":"anthropic","models":{},` +
				`"subagents":{"default_seat":"worker","models":{}}}}}`)
			req := httptest.NewRequest(
				http.MethodPost,
				"/v1/messages",
				strings.NewReader(`{"model":"claude-sonnet-5"}`),
			)
			req.Header.Set(ClaudeCodeAgentIDHeader, "agent")
			rec := httptest.NewRecorder()
			newSubagentProxy(t, newSubagentTestRegistry(t, body)).ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func writeSubagentTestFile(t *testing.T, envName, contents string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envName, path)
}

func newSubagentTestRegistry(t *testing.T, body []byte) *registry.Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.New(path)
	if err != nil {
		t.Fatalf("registry.New() = %v\n%s", err, body)
	}
	return reg
}
