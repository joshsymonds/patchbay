package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/server"
	"github.com/joshsymonds/patchbay/internal/testlog"
)

const (
	introspectPath = "/_patchbay/registry"
	callerKey      = "s3cret-caller-key"
	listenAddr     = "127.0.0.1:4100"
)

func callerKeyFile(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "caller.key")
	if err := os.WriteFile(path, []byte(callerKey), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(proxy.CallerKeyEnvFile, path)
}

func introspectRequest(key string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, introspectPath, nil)
	if key != "" {
		req.Header.Set(proxy.CallerKeyHeader, key)
	}
	return req
}

func decodeDocument(t *testing.T, body []byte) server.Document {
	t.Helper()
	var doc server.Document
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestIntrospectionReturnsSeatSummariesContextBindingsAndDefault(t *testing.T) {
	callerKeyFile(t)
	body := strings.Replace(
		modelRegistry("personal"),
		`"luna":{"upstream"`,
		`"luna":{"extra_body":{"secretish":"must-not-appear"},"upstream"`,
		1,
	)
	rec := serve(
		t,
		server.Config{
			Proxy:    newSentinel(),
			Registry: newRegistryDocument(t, body),
			Listen:   listenAddr,
		},
		introspectRequest(callerKey),
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	doc := decodeDocument(t, rec.Body.Bytes())
	if !doc.RegistryValid || doc.RegistryError != "" || doc.DefaultContext == nil ||
		*doc.DefaultContext != "personal" ||
		doc.Listen != listenAddr {
		t.Fatalf("doc=%+v", doc)
	}
	if len(doc.Seats) != 5 || doc.Seats["luna"].AuthMode != "inject" ||
		doc.Seats["luna"].Upstream != "https://openrouter.ai/api" {
		t.Fatalf("seats=%+v", doc.Seats)
	}
	personal := doc.Contexts["personal"]
	if personal.DefaultSeat != "anthropic" || personal.Models["luna-fast"] != "luna" ||
		personal.Subagents == nil ||
		personal.Subagents.DefaultSeat != "sub" {
		t.Fatalf("context=%+v", personal)
	}
	for _, secret := range []string{"must-not-appear", callerKey, "arn:"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("secret %q exposed: %s", secret, rec.Body.String())
		}
	}
}

// rejectedValue is the distinctive string the invalid replacement below names in
// a field validation quotes back, standing in for whatever an operator pasted
// into the wrong place. Introspection is caller-key-gated, not operator-only:
// the key is a machine-local secret many processes hold, so the document says
// the registry is invalid without republishing what the rejected file contains.
const rejectedValue = "sk-live-secret"

// secretRegistry is an invalid registry whose validation error quotes
// rejectedValue: api_key_env_file names a value that is not an environment
// variable name at all.
const secretRegistry = `{"default_context":"personal","seats":{` +
	`"luna":{"upstream":"https://openrouter.ai/api","model":"openai/gpt-5.6-luna",` +
	`"auth_mode":"inject","api_key_env_file":"` + rejectedValue + `"}},` +
	`"contexts":{"personal":{"default_seat":"luna","models":{}}}}`

func TestUnprefixedIntrospectionReportsHotInvalidSentinel(t *testing.T) {
	callerKeyFile(t)
	reg := newRegistryDocument(t, modelRegistry("personal"))
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(reg.Path(), base, base); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reg.Path(), []byte(secretRegistry), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(reg.Path(), base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	rec := serve(
		t,
		server.Config{Proxy: newSentinel(), Registry: reg},
		introspectRequest(callerKey),
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	doc := decodeDocument(t, rec.Body.Bytes())
	if doc.RegistryValid || doc.RegistryError == "" || doc.DefaultContext != nil ||
		len(doc.Seats) != 0 ||
		len(doc.Contexts) != 0 {
		t.Fatalf("doc=%+v", doc)
	}
	if strings.Contains(doc.RegistryError, reg.Path()) {
		t.Fatalf("unsanitized error=%q", doc.RegistryError)
	}
	// registry_path is the document's own field and stays; what the rejected file
	// said must appear nowhere in the answer.
	if strings.Contains(rec.Body.String(), rejectedValue) {
		t.Fatalf("invalid document republishes a rejected value: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"validation_error"`) ||
		strings.Contains(rec.Body.String(), `"registry_error"`) {
		t.Fatalf("invalid registry wire fields=%s", rec.Body.String())
	}
}

// TestBareIntrospectionReportsTheInvalidRegistryOnlyForAKeyedCaller pins the one
// invalid state introspection sees first. Bare "/_patchbay/registry" is answered
// by the outer wrapper before any context is selected, so nothing else has
// observed the failure yet and this is the only place the operator can hear
// about it.
//
// The report sits behind the caller-key gate rather than in front of it. What is
// logged is the detail the rejected file contains, and an unauthenticated prober
// must not be able to make the gateway write it anywhere — the answer such a
// caller gets is 403 and a silent log.
func TestBareIntrospectionReportsTheInvalidRegistryOnlyForAKeyedCaller(t *testing.T) {
	callerKeyFile(t)
	for _, tc := range []struct {
		name, key    string
		want, report int
	}{
		{name: "no key", key: "", want: http.StatusForbidden},
		{name: "wrong key", key: "wrong", want: http.StatusForbidden},
		{name: "the caller key", key: callerKey, want: http.StatusOK, report: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := newRegistryDocument(t, modelRegistry("personal"))
			base := time.Now().Add(-time.Hour).Truncate(time.Second)
			if err := os.Chtimes(reg.Path(), base, base); err != nil {
				t.Fatal(err)
			}
			replaceRegistryFile(t, reg.Path(), secretRegistry, base.Add(time.Hour))

			sink := testlog.New(callBuffer)
			rec := serve(t, server.Config{
				Proxy:    newSentinel(),
				Registry: reg,
				Logger:   sink.Logger(),
			}, introspectRequest(tc.key))

			if rec.Code != tc.want {
				t.Fatalf("status = %d; want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), rejectedValue) {
				t.Errorf("the answer republished a rejected value: %s", rec.Body.String())
			}

			got := sink.Matching(operatorInvalidMessage)
			if len(got) != tc.report {
				t.Fatalf("operator log holds %d %q entries; want %d\n%s",
					len(got), operatorInvalidMessage, tc.report, sink)
			}
			if tc.report == 0 {
				return
			}

			if !got[0].OperatorVisible() {
				t.Errorf("operator log %q is quieter than a warning", got[0].Text())
			}
			for _, detail := range []string{reg.Path(), rejectedValue, "api_key_env_file"} {
				if !strings.Contains(got[0].Attrs(), detail) {
					t.Errorf("operator log %q keeps no %q", got[0].Text(), detail)
				}
			}
			if doc := decodeDocument(t, rec.Body.Bytes()); doc.RegistryValid ||
				doc.RegistryError == "" {
				t.Fatalf("doc = %+v; want the sanitized invalid document", doc)
			}
		})
	}
}

// TestIntrospectionOnAValidRegistryForgetsTheReportedFailure is the other half
// of the document path. Introspection reads the same state a request resolves
// from, so a valid one is evidence the reported failure is over — and a file
// that breaks the same way after that is a failure nobody has been told about
// yet, on this surface exactly as on the request path.
func TestIntrospectionOnAValidRegistryForgetsTheReportedFailure(t *testing.T) {
	callerKeyFile(t)
	reg := newRegistryDocument(t, modelRegistry("personal"))
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(reg.Path(), base, base); err != nil {
		t.Fatal(err)
	}

	sink := testlog.New(callBuffer)
	// One handler throughout: what it has already reported is the thing under test.
	handler := server.Handler(server.Config{
		Proxy:    newSentinel(),
		Registry: reg,
		Logger:   sink.Logger(),
	})
	ask := func() server.Document {
		t.Helper()

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, introspectRequest(callerKey))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body %s)", rec.Code, rec.Body.String())
		}

		return decodeDocument(t, rec.Body.Bytes())
	}
	reported := func(want int) {
		t.Helper()

		if got := sink.Matching(operatorInvalidMessage); len(got) != want {
			t.Fatalf("operator log holds %d %q entries; want %d\n%s",
				len(got), operatorInvalidMessage, want, sink)
		}
	}

	replaceRegistryFile(t, reg.Path(), secretRegistry, base.Add(time.Hour))

	if doc := ask(); doc.RegistryValid {
		t.Fatalf("doc = %+v; want the invalid document", doc)
	}
	reported(1)

	replaceRegistryFile(t, reg.Path(), modelRegistry("personal"), base.Add(2*time.Hour))

	if doc := ask(); !doc.RegistryValid {
		t.Fatalf("doc = %+v; want the repaired document", doc)
	}
	reported(1)

	replaceRegistryFile(t, reg.Path(), secretRegistry, base.Add(3*time.Hour))

	if doc := ask(); doc.RegistryValid {
		t.Fatalf("doc = %+v; want the invalid document again", doc)
	}
	reported(2)
}

func TestIntrospectionRemainsCallerKeyGated(t *testing.T) {
	callerKeyFile(t)
	for _, key := range []string{"", "wrong"} {
		rec := serve(
			t,
			server.Config{
				Proxy:    newSentinel(),
				Registry: newRegistryDocument(t, modelRegistry("personal")),
			},
			introspectRequest(key),
		)
		if rec.Code != http.StatusForbidden {
			t.Errorf("key %q status=%d", key, rec.Code)
		}
	}
}

// TestIntrospectionMethodsAndPaths pins the surface patchbay claims for itself.
// The prefix is patchbay's own — no Anthropic endpoint lives under it — so a path
// nobody serves is answered here rather than becoming a request to Anthropic the
// caller never made.
func TestIntrospectionMethodsAndPaths(t *testing.T) {
	callerKeyFile(t)
	for _, tc := range []struct {
		name, method, target string
		want                 int
		wantAllow            bool
	}{
		{name: "get", method: http.MethodGet, target: introspectPath, want: http.StatusOK},
		{name: "head", method: http.MethodHead, target: introspectPath, want: http.StatusOK},
		{name: "post", method: http.MethodPost, target: introspectPath, want: http.StatusMethodNotAllowed, wantAllow: true},
		{name: "delete", method: http.MethodDelete, target: introspectPath, want: http.StatusMethodNotAllowed, wantAllow: true},
		{name: "unknown sibling", method: http.MethodGet, target: "/_patchbay/nothing", want: http.StatusNotFound},
		{name: "bare root", method: http.MethodGet, target: "/_patchbay", want: http.StatusNotFound},
		{name: "deep unknown", method: http.MethodGet, target: "/_patchbay/a/b/c", want: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pipeline := newSentinel()
			req := httptest.NewRequest(tc.method, tc.target, nil)
			req.Header.Set(proxy.CallerKeyHeader, callerKey)
			rec := serve(t, server.Config{
				Proxy:    pipeline,
				Registry: newRegistryDocument(t, modelRegistry("personal")),
			}, req)

			if rec.Code != tc.want {
				t.Errorf("status = %d; want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.wantAllow && rec.Header().Get("Allow") == "" {
				t.Error("a 405 carries no Allow header")
			}
			if pipeline.hits() != 0 {
				t.Error("an introspection path reached the proxy")
			}
		})
	}
}

// TestIntrospectionAnswersUnderAContextPrefix pins the prefixed form. A context
// cannot put an Anthropic endpoint under patchbay's own prefix, so
// "/ctx/work/_patchbay/..." is answered here exactly as the bare form is.
func TestIntrospectionAnswersUnderAContextPrefix(t *testing.T) {
	callerKeyFile(t)
	pipeline := newSentinel()
	rec := serve(t, server.Config{
		Proxy:    pipeline,
		Registry: newRegistryDocument(t, modelRegistry("personal")),
		Listen:   listenAddr,
	}, keyedIntrospectRequest("/ctx/personal"+introspectPath))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if doc := decodeDocument(t, rec.Body.Bytes()); !doc.RegistryValid {
		t.Fatalf("doc = %+v", doc)
	}
	if pipeline.hits() != 0 {
		t.Error("a prefixed introspection path reached the proxy")
	}

	unknown := newSentinel()
	unknownRec := serve(t, server.Config{
		Proxy:    unknown,
		Registry: newRegistryDocument(t, modelRegistry("personal")),
	}, keyedIntrospectRequest("/ctx/personal/_patchbay/nothing"))
	if unknownRec.Code != http.StatusNotFound || unknown.hits() != 0 {
		t.Fatalf("prefixed unknown path status = %d hits = %d", unknownRec.Code, unknown.hits())
	}
}

func keyedIntrospectRequest(target string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set(proxy.CallerKeyHeader, callerKey)
	return req
}

// TestIntrospectionReportsAnUnreadableCallerKey separates the server's own
// misconfiguration from a caller's refusal: 500, not 403.
func TestIntrospectionReportsAnUnreadableCallerKey(t *testing.T) {
	unreadableCallerKey(t)
	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistryDocument(t, modelRegistry("personal")),
	}, introspectRequest(callerKey))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestIntrospectionOmitsFieldsASeatDoesNotUse keeps the summary honest about
// each auth mode rather than publishing empty AWS fields on every Seat.
func TestIntrospectionOmitsFieldsASeatDoesNotUse(t *testing.T) {
	callerKeyFile(t)
	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistryDocument(t, modelRegistry("personal")),
		Listen:   listenAddr,
	}, introspectRequest(callerKey))

	var raw struct {
		Seats map[string]map[string]any `json:"seats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"aws_profile", "aws_region", "model_map_env_file", "api_key_env_file"} {
		if _, present := raw.Seats["anthropic"][absent]; present {
			t.Errorf("forward Seat publishes %q: %v", absent, raw.Seats["anthropic"])
		}
	}
	for _, absent := range []string{"aws_profile", "aws_region", "model_map_env_file"} {
		if _, present := raw.Seats["luna"][absent]; present {
			t.Errorf("inject Seat publishes %q: %v", absent, raw.Seats["luna"])
		}
	}
	if raw.Seats["bedrock"]["upstream"] != "https://bedrock-runtime.us-east-2.amazonaws.com" {
		t.Errorf("sigv4 Seat upstream = %v; want the endpoint its region derives", raw.Seats["bedrock"]["upstream"])
	}
	if _, present := raw.Seats["bedrock"]["api_key_env_file"]; present {
		t.Errorf("sigv4 Seat publishes api_key_env_file: %v", raw.Seats["bedrock"])
	}
}
