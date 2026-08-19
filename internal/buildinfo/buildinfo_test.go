package buildinfo_test

import (
	"testing"

	"github.com/joshsymonds/patchbay/internal/buildinfo"
)

func TestVersionIsNonEmpty(t *testing.T) {
	t.Parallel()

	if got := buildinfo.Version(); got == "" {
		t.Fatalf("Version() returned an empty string; want a non-empty version identifier")
	}
}
