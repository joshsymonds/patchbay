package awssig_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/joshsymonds/patchbay/internal/awssig"
)

// awsLogEnv names the file the fake aws binary records its invocations in. It
// is read by the script itself, so counting execs needs no clock and no hook
// inside the package under test.
const awsLogEnv = "PATCHBAY_TEST_AWS_LOG"

// fakeAWS installs a script named aws as the only executable on PATH, running
// body, and returns the path of the log every invocation appends its arguments
// to.
func fakeAWS(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")

	// Each invocation logs the profile the CLI was pointed at, which is how the
	// tests judge selection: the profile arrives in the environment, not the
	// command line.
	script := "#!/bin/sh\necho \"$AWS_PROFILE $@\" >> \"$" + awsLogEnv + "\"\n" + body

	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake aws: %v", err)
	}

	t.Setenv(awsLogEnv, logPath)
	// Prepended rather than replacing PATH: the script itself needs the ordinary
	// shell utilities, and being first is what makes it the aws that runs.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return logPath
}

// exportBody is a fake aws that prints one credential document.
func exportBody(expiration string) string {
	document := `{"Version":1,"AccessKeyId":"AKIAFAKE","SecretAccessKey":"fake-secret","SessionToken":"fake-token"`
	if expiration != "" {
		document += `,"Expiration":"` + expiration + `"`
	}

	return "cat <<'JSON'\n" + document + "}\nJSON\n"
}

// callLog returns the recorded invocations, one line per exec.
func callLog(t *testing.T, logPath string) []string {
	t.Helper()

	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		t.Fatalf("read call log: %v", err)
	}

	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}

	return strings.Split(trimmed, "\n")
}

func TestCredentialsExportsFromTheProfile(t *testing.T) {
	logPath := fakeAWS(t, exportBody("2099-01-01T00:00:00Z"))

	creds, _, err := awssig.NewCredentialSource().Credentials(t.Context(), "work")
	if err != nil {
		t.Fatalf("Credentials() = %v; want the exported credentials", err)
	}

	if creds.AccessKeyID != "AKIAFAKE" {
		t.Errorf("AccessKeyID = %q; want %q", creds.AccessKeyID, "AKIAFAKE")
	}

	if creds.SecretAccessKey != "fake-secret" {
		t.Errorf("SecretAccessKey = %q; want %q", creds.SecretAccessKey, "fake-secret")
	}

	if creds.SessionToken != "fake-token" {
		t.Errorf("SessionToken = %q; want %q", creds.SessionToken, "fake-token")
	}

	calls := callLog(t, logPath)
	if len(calls) != 1 {
		t.Fatalf("aws ran %d times; want 1", len(calls))
	}

	if want := "work configure export-credentials"; calls[0] != want {
		t.Errorf("aws ran %q; want %q", calls[0], want)
	}
}

func TestCredentialsAreCachedAndRefreshed(t *testing.T) {
	tests := []struct {
		name       string
		expiration string
		wantExecs  int
	}{
		{name: "far future expiry is cached", expiration: "2099-01-01T00:00:00Z", wantExecs: 1},
		{name: "no expiry is cached", expiration: "", wantExecs: 1},
		{name: "expired credentials are refreshed", expiration: "2000-01-01T00:00:00Z", wantExecs: 3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logPath := fakeAWS(t, exportBody(tc.expiration))
			source := awssig.NewCredentialSource()

			for range 3 {
				if _, _, err := source.Credentials(t.Context(), "work"); err != nil {
					t.Fatalf("Credentials() = %v; want the exported credentials", err)
				}
			}

			if got := len(callLog(t, logPath)); got != tc.wantExecs {
				t.Errorf("aws ran %d times; want %d", got, tc.wantExecs)
			}
		})
	}
}

// A credential still inside the two-minute refresh skew is treated as expired:
// a signature made with it could easily land after it dies.
func TestCredentialsRefreshInsideTheSkew(t *testing.T) {
	logPath := fakeAWS(t, "cat <<JSON\n"+
		`{"AccessKeyId":"AKIAFAKE","SecretAccessKey":"fake-secret","SessionToken":"fake-token",`+
		`"Expiration":"$(date -u -d '+1 minute' +%Y-%m-%dT%H:%M:%SZ)"}`+"\nJSON\n")

	source := awssig.NewCredentialSource()

	for range 2 {
		if _, _, err := source.Credentials(t.Context(), "work"); err != nil {
			t.Fatalf("Credentials() = %v; want the exported credentials", err)
		}
	}

	if got := len(callLog(t, logPath)); got != 2 {
		t.Errorf("aws ran %d times; want 2 — a credential expiring inside the skew is not reused", got)
	}
}

// Concurrent requests that all need a refresh share one exec. Without that,
// every in-flight turn would spawn its own aws process on a cold cache.
func TestCredentialsRefreshIsSingleFlight(t *testing.T) {
	logPath := fakeAWS(t, "sleep 0.3\n"+exportBody("2099-01-01T00:00:00Z"))
	source := awssig.NewCredentialSource()

	var group sync.WaitGroup

	for range 8 {
		group.Add(1)

		go func() {
			defer group.Done()

			if _, _, err := source.Credentials(t.Context(), "work"); err != nil {
				t.Errorf("Credentials() = %v; want the exported credentials", err)
			}
		}()
	}

	group.Wait()

	if got := len(callLog(t, logPath)); got != 1 {
		t.Errorf("aws ran %d times; want exactly 1 for a shared refresh", got)
	}
}

// Invalidate is what a 403 costs: the cached credential is dropped, so the
// retry signs with a freshly exported one.
func TestInvalidateForcesARefresh(t *testing.T) {
	logPath := fakeAWS(t, exportBody("2099-01-01T00:00:00Z"))
	source := awssig.NewCredentialSource()

	_, generation, err := source.Credentials(t.Context(), "work")
	if err != nil {
		t.Fatalf("Credentials() = %v; want the exported credentials", err)
	}

	source.Invalidate("work", generation)

	if _, _, err := source.Credentials(t.Context(), "work"); err != nil {
		t.Fatalf("Credentials() = %v; want the exported credentials", err)
	}

	if got := len(callLog(t, logPath)); got != 2 {
		t.Errorf("aws ran %d times; want 2 after an invalidation", got)
	}
}

// Each profile is cached on its own, so one context's credentials never answer
// for another's.
func TestCredentialsAreCachedPerProfile(t *testing.T) {
	logPath := fakeAWS(t, exportBody("2099-01-01T00:00:00Z"))
	source := awssig.NewCredentialSource()

	for _, profile := range []string{"work", "personal", "work"} {
		if _, _, err := source.Credentials(t.Context(), profile); err != nil {
			t.Fatalf("Credentials(%q) = %v; want the exported credentials", profile, err)
		}
	}

	calls := callLog(t, logPath)
	if len(calls) != 2 {
		t.Fatalf("aws ran %d times; want 2 — one per profile", len(calls))
	}

	if !strings.HasPrefix(calls[1], "personal ") {
		t.Errorf("second call = %q; want it to name the personal profile", calls[1])
	}
}

func TestCredentialsFailuresAreSanitized(t *testing.T) {
	const stderrSecret = "ASIA-LEAKED-KEY-MATERIAL"

	tests := []struct {
		name string
		body string
	}{
		{name: "non-zero exit", body: "echo " + stderrSecret + " >&2\nexit 1\n"},
		{name: "unparsable output", body: "echo not-json\n"},
		{name: "empty credentials", body: "echo '{\"AccessKeyId\":\"\"}'\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeAWS(t, tc.body)

			_, _, err := awssig.NewCredentialSource().Credentials(t.Context(), "work")
			if err == nil {
				t.Fatal("Credentials() = nil; want an error")
			}

			if strings.Contains(err.Error(), stderrSecret) {
				t.Errorf("error = %q; want it to carry no output from the aws process", err.Error())
			}

			if !strings.Contains(err.Error(), "work") {
				t.Errorf("error = %q; want it to name the profile", err.Error())
			}
		})
	}
}

func TestCredentialsWithoutTheAWSBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	if _, _, err := awssig.NewCredentialSource().Credentials(t.Context(), "work"); err == nil {
		t.Fatal("Credentials() = nil; want an error when aws is not on PATH")
	}
}

// A 403 invalidates the credential that SIGNED it, not whichever one the cache
// holds by the time the refusal arrives. Without that, k concurrent turns that
// all fail on one expired credential each throw away the fresh one the turn
// before them just exported, and each pays for its own aws process.
func TestInvalidateIgnoresAStaleGeneration(t *testing.T) {
	// The export is slow enough that the goroutines below genuinely overlap it,
	// which is the interleaving a per-request invalidation gets wrong.
	logPath := fakeAWS(t, "sleep 0.2\n"+exportBody("2099-01-01T00:00:00Z"))
	source := awssig.NewCredentialSource()

	_, generation, err := source.Credentials(t.Context(), "work")
	if err != nil {
		t.Fatalf("Credentials() = %v; want the exported credentials", err)
	}

	var group sync.WaitGroup

	for range 8 {
		group.Add(1)

		go func() {
			defer group.Done()

			// Every one of them was refused while signing with the same, now
			// stale, credential.
			source.Invalidate("work", generation)

			if _, _, retryErr := source.Credentials(t.Context(), "work"); retryErr != nil {
				t.Errorf("Credentials() = %v; want the exported credentials", retryErr)
			}
		}()
	}

	group.Wait()

	if got := len(callLog(t, logPath)); got != 2 {
		t.Errorf("aws ran %d times; want 2 — one export, then exactly one re-export", got)
	}
}

// A generation nothing has refused leaves the cache alone, so a stale
// invalidation cannot cost a credential that is still good.
func TestInvalidateLeavesANewerGenerationAlone(t *testing.T) {
	logPath := fakeAWS(t, exportBody("2099-01-01T00:00:00Z"))
	source := awssig.NewCredentialSource()

	_, first, err := source.Credentials(t.Context(), "work")
	if err != nil {
		t.Fatalf("Credentials() = %v; want the exported credentials", err)
	}

	source.Invalidate("work", first)

	_, second, refreshErr := source.Credentials(t.Context(), "work")
	if refreshErr != nil {
		t.Fatalf("Credentials() = %v; want the exported credentials", refreshErr)
	}

	if second == first {
		t.Fatalf("generation = %d after a refresh; want a new one", second)
	}

	// The refusal that produced `first` arrives late; the credential it names is
	// gone, and the fresh one must survive it.
	source.Invalidate("work", first)

	if _, _, reuseErr := source.Credentials(t.Context(), "work"); reuseErr != nil {
		t.Fatalf("Credentials() = %v; want the cached credentials", reuseErr)
	}

	if got := len(callLog(t, logPath)); got != 2 {
		t.Errorf("aws ran %d times; want 2 — a stale invalidation exports nothing", got)
	}
}
