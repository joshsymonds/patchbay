//go:build darwin

package usage

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// TestDescriptorHasExtendedACLBindsRealLibSystem exercises the real purego
// libSystem binding rather than the injected doubles above: it proves the
// symbols resolve, the calling convention matches, and a freshly created
// descriptor with no ACL is reported as trivial instead of failing closed.
func TestDescriptorHasExtendedACLBindsRealLibSystem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "clean.sqlite")
	if err := os.WriteFile(file, []byte("clean"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, target := range []struct {
		name  string
		path  string
		flags int
	}{
		{name: "regular file", path: file, flags: syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC},
		{
			name:  "directory",
			path:  dir,
			flags: syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC,
		},
	} {
		t.Run(target.name, func(t *testing.T) {
			t.Parallel()

			fd, openErr := syscall.Open(target.path, target.flags, 0)
			if openErr != nil {
				t.Fatalf("open %q: %v", target.path, openErr)
			}
			defer func() {
				if closeErr := syscall.Close(fd); closeErr != nil {
					t.Errorf("close %q: %v", target.path, closeErr)
				}
			}()

			hasACL, err := descriptorHasExtendedACL(fd)
			if err != nil {
				t.Fatalf("descriptorHasExtendedACL(%q) error = %v; want nil", target.path, err)
			}
			if hasACL {
				t.Errorf("descriptorHasExtendedACL(%q) = true; want false for a freshly created entry", target.path)
			}
		})
	}
}

// TestBindDarwinACLFunctionsReleasesLibraryOnFailure proves a failed binding does
// not strand libSystem. Binding happens per inspection, so a handle leaked on the
// failure path would accumulate a reference on every rejected database open with
// nothing left able to release it. A successful bind returns its handle still open
// on purpose: the registered trampolines call into that library until
// descriptorHasExtendedACL closes it.
func TestBindDarwinACLFunctionsReleasesLibraryOnFailure(t *testing.T) {
	t.Parallel()

	const handle = uintptr(0x4c1b)
	wantSymbols := []string{"acl_get_fd_np", "acl_get_entry", "acl_free", "__error"}

	for _, test := range []struct {
		name string
		// configure may adjust open, symbol, and register. It must not replace
		// closeLib: the counting stub below is what proves the release happened,
		// and an override would silently discard it. Drive close failure through
		// closeErr instead.
		configure  func(*darwinSymbolLoader)
		closeErr   error
		wantErr    []string
		wantCloses int
		wantHandle uintptr
	}{
		{
			name: "library does not open",
			configure: func(loader *darwinSymbolLoader) {
				loader.open = func(string, int) (uintptr, error) { return 0, syscall.ENOENT }
			},
			wantErr: []string{"open libSystem for ACL inspection", "no such file"},
		},
		{
			name: "symbol does not resolve",
			configure: func(loader *darwinSymbolLoader) {
				loader.symbol = func(_ uintptr, name string) (uintptr, error) {
					if name == "acl_free" {
						return 0, syscall.EINVAL
					}
					return 1, nil
				}
			},
			wantErr:    []string{"resolve acl_free", "invalid argument"},
			wantCloses: 1,
		},
		{
			name: "registration panics",
			configure: func(loader *darwinSymbolLoader) {
				loader.register = func(any, uintptr) { panic("unsupported calling convention") }
			},
			wantErr:    []string{"register Darwin ACL functions", "unsupported calling convention"},
			wantCloses: 1,
		},
		{
			name: "cleanup failure joins the original error",
			configure: func(loader *darwinSymbolLoader) {
				loader.symbol = func(_ uintptr, name string) (uintptr, error) {
					if name == "__error" {
						return 0, syscall.EINVAL
					}
					return 1, nil
				}
			},
			closeErr:   syscall.EIO,
			wantErr:    []string{"resolve __error", "close libSystem after failed ACL binding", "input/output error"},
			wantCloses: 1,
		},
		{
			name:       "successful bind retains the handle",
			configure:  func(*darwinSymbolLoader) {},
			wantHandle: handle,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			closes := 0
			var requested []string
			loader := darwinSymbolLoader{
				open: func(path string, mode int) (uintptr, error) {
					if path != "/usr/lib/libSystem.B.dylib" {
						t.Errorf("Dlopen(%q); want libSystem", path)
					}
					if mode == 0 {
						t.Error("Dlopen mode = 0; want RTLD flags")
					}
					return handle, nil
				},
				symbol: func(library uintptr, name string) (uintptr, error) {
					if library != handle {
						t.Errorf("Dlsym library = %#x; want %#x", library, handle)
					}
					requested = append(requested, name)
					return 1, nil
				},
				closeLib: func(library uintptr) error {
					if library != handle {
						t.Errorf("Dlclose library = %#x; want %#x", library, handle)
					}
					closes++
					return test.closeErr
				},
				register: func(target any, address uintptr) {
					if address == 0 {
						t.Error("RegisterFunc address = 0; want a resolved symbol")
					}
					// Each target must be a pointer to exactly the signature its
					// C function uses; a miswired field would be rejected here as
					// a signature assignDarwinACLStub does not know.
					if !assignDarwinACLStub(target) {
						t.Errorf("RegisterFunc target %T is not an ACL binding signature", target)
					}
				},
			}
			test.configure(&loader)

			library, bindings, err := bindDarwinACLFunctions(loader)

			if len(test.wantErr) == 0 {
				assertDarwinACLBindSucceeded(t, bindings, err, requested, wantSymbols)
			} else {
				assertDarwinACLBindFailed(t, bindings, err, test.wantErr)
			}
			if closes != test.wantCloses {
				t.Errorf("Dlclose calls = %d; want %d", closes, test.wantCloses)
			}
			if library != test.wantHandle {
				t.Errorf("bindDarwinACLFunctions() library = %#x; want %#x", library, test.wantHandle)
			}
		})
	}
}

func assertDarwinACLBindSucceeded(
	t *testing.T,
	bindings darwinACLBindings,
	err error,
	requested, wantSymbols []string,
) {
	t.Helper()

	if err != nil {
		t.Fatalf("bindDarwinACLFunctions() error = %v; want nil", err)
	}
	if !reflect.DeepEqual(requested, wantSymbols) {
		t.Errorf("resolved symbols = %v; want %v", requested, wantSymbols)
	}
	if bindings.getFD == nil || bindings.getEntry == nil ||
		bindings.free == nil || bindings.errnoLocation == nil {
		t.Error("bindDarwinACLFunctions() returned an incompletely bound ACL surface")
	}
}

func assertDarwinACLBindFailed(
	t *testing.T,
	bindings darwinACLBindings,
	err error,
	wantErr []string,
) {
	t.Helper()

	if err == nil {
		t.Fatalf("bindDarwinACLFunctions() error = nil; want %v", wantErr)
	}
	for _, want := range wantErr {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("bindDarwinACLFunctions() error = %v; want it to mention %q", err, want)
		}
	}
	if bindings.getFD != nil || bindings.getEntry != nil ||
		bindings.free != nil || bindings.errnoLocation != nil {
		t.Error("bindDarwinACLFunctions() returned partial bindings alongside an error")
	}
}

// assignDarwinACLStub wires target to a no-op implementation of the C signature it
// declares, reporting whether target was one of the four ACL binding signatures.
// A miswired binding field arrives here as a type this function does not know.
func assignDarwinACLStub(target any) bool {
	switch binding := target.(type) {
	case *func(int32, int32) uintptr:
		*binding = func(int32, int32) uintptr { return 0 }
	case *func(uintptr, int32, *uintptr) int32:
		*binding = func(uintptr, int32, *uintptr) int32 { return 0 }
	case *func(uintptr) int32:
		*binding = func(uintptr) int32 { return 0 }
	case *func() *int32:
		*binding = func() *int32 { return nil }
	default:
		return false
	}
	return true
}

// TestInspectWithDarwinACLLibraryLifecycle pins the whole open/inspect/close
// lifecycle, not just the binding half. Every outcome an inspection can reach —
// success, a returned error, and a panic out of a purego trampoline — must release
// libSystem exactly once, and a failed release must fail the inspection closed
// rather than report an ACL verdict nothing can vouch for.
//
// The close count is the mutation-resistant part: deleting the cleanup from
// inspectWithDarwinACLLibrary drops every wantCloses to 0, and dropping the
// recover turns the panic case into a test-binary crash.
func TestInspectWithDarwinACLLibraryLifecycle(t *testing.T) {
	t.Parallel()

	const (
		handle       = uintptr(0x115e)
		fd           = 42
		inspectFails = "inspect failed"
		trampoline   = "purego trampoline exploded"
	)

	for _, test := range []struct {
		name         string
		closeErr     error
		inspect      darwinACLInspector
		openErr      error
		symbolErr    error
		wantACL      bool
		wantErr      []string
		wantCloses   int
		wantInspects int
	}{
		{
			name:         "successful inspection releases the library once",
			inspect:      func(int, darwinACLBindings) (bool, error) { return true, nil },
			wantACL:      true,
			wantCloses:   1,
			wantInspects: 1,
		},
		{
			name: "returned inspection error still releases the library once",
			inspect: func(int, darwinACLBindings) (bool, error) {
				return false, errors.New(inspectFails)
			},
			wantErr:      []string{inspectFails},
			wantCloses:   1,
			wantInspects: 1,
		},
		{
			name:         "panicking inspection fails closed and releases the library once",
			inspect:      func(int, darwinACLBindings) (bool, error) { panic(trampoline) },
			wantErr:      []string{"inspect Darwin descriptor ACL", trampoline},
			wantCloses:   1,
			wantInspects: 1,
		},
		{
			name:     "close failure after a positive verdict fails closed",
			closeErr: syscall.EIO,
			inspect:  func(int, darwinACLBindings) (bool, error) { return true, nil },
			// The verdict was "has ACL", but the library did not release. Reporting
			// true here would hand path validation a result from an unaccounted
			// library handle, so the lifecycle downgrades it to a refusal.
			wantErr: []string{
				"close libSystem after Darwin ACL inspection",
				"input/output error",
			},
			wantCloses:   1,
			wantInspects: 1,
		},
		{
			name:     "close failure joins the returned inspection error",
			closeErr: syscall.EIO,
			inspect: func(int, darwinACLBindings) (bool, error) {
				return false, errors.New(inspectFails)
			},
			wantErr: []string{
				inspectFails,
				"close libSystem after Darwin ACL inspection",
			},
			wantCloses:   1,
			wantInspects: 1,
		},
		{
			name:     "close failure joins the recovered panic",
			closeErr: syscall.EIO,
			inspect:  func(int, darwinACLBindings) (bool, error) { panic(trampoline) },
			wantErr: []string{
				trampoline,
				"close libSystem after Darwin ACL inspection",
			},
			wantCloses:   1,
			wantInspects: 1,
		},
		{
			name:    "a library that never opens is never inspected or closed",
			openErr: syscall.ENOENT,
			inspect: func(int, darwinACLBindings) (bool, error) {
				t.Error("inspection ran despite a failed Dlopen")
				return true, nil
			},
			wantErr: []string{"open libSystem for ACL inspection"},
		},
		{
			name:      "bind-failure cleanup keeps its own message",
			symbolErr: syscall.EINVAL,
			closeErr:  syscall.EIO,
			inspect: func(int, darwinACLBindings) (bool, error) {
				t.Error("inspection ran despite a failed symbol resolution")
				return true, nil
			},
			// Distinct from the inspection-cleanup wording above: the two closes
			// happen at different points and must stay tellable apart in a log.
			wantErr: []string{
				"resolve acl_get_fd_np",
				"close libSystem after failed ACL binding",
			},
			wantCloses: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			closes := 0
			inspects := 0
			loader := darwinSymbolLoader{
				open: func(string, int) (uintptr, error) {
					if test.openErr != nil {
						return 0, test.openErr
					}
					return handle, nil
				},
				symbol: func(_ uintptr, name string) (uintptr, error) {
					if test.symbolErr != nil {
						return 0, fmt.Errorf("%s: %w", name, test.symbolErr)
					}
					return 1, nil
				},
				closeLib: func(library uintptr) error {
					if library != handle {
						t.Errorf("Dlclose library = %#x; want %#x", library, handle)
					}
					closes++
					return test.closeErr
				},
				register: func(target any, _ uintptr) {
					if !assignDarwinACLStub(target) {
						t.Errorf("RegisterFunc target %T is not an ACL binding signature", target)
					}
				},
			}
			inspect := func(gotFD int, bindings darwinACLBindings) (bool, error) {
				inspects++
				if gotFD != fd {
					t.Errorf("inspection fd = %d; want %d", gotFD, fd)
				}
				if bindings.getFD == nil || bindings.errnoLocation == nil {
					t.Error("inspection received incompletely bound ACL functions")
				}
				return test.inspect(gotFD, bindings)
			}

			hasACL, err := inspectWithDarwinACLLibrary(fd, loader, inspect)

			if hasACL != test.wantACL {
				t.Errorf("inspectWithDarwinACLLibrary() hasACL = %t; want %t", hasACL, test.wantACL)
			}
			assertDarwinACLLifecycleError(t, err, test.wantErr)
			if closes != test.wantCloses {
				t.Errorf("Dlclose calls = %d; want %d", closes, test.wantCloses)
			}
			if inspects != test.wantInspects {
				t.Errorf("inspection calls = %d; want %d", inspects, test.wantInspects)
			}
		})
	}
}

func assertDarwinACLLifecycleError(t *testing.T, err error, wantErr []string) {
	t.Helper()

	if len(wantErr) == 0 {
		if err != nil {
			t.Errorf("inspectWithDarwinACLLibrary() error = %v; want nil", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("inspectWithDarwinACLLibrary() error = nil; want %v", wantErr)
	}
	for _, want := range wantErr {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("inspectWithDarwinACLLibrary() error = %v; want it to mention %q", err, want)
		}
	}
}

// TestDescriptorHasExtendedACLUsesTheSystemLifecycle proves the exported entry
// point is the lifecycle above driven by the real loader, so the coverage in
// TestInspectWithDarwinACLLibraryLifecycle describes production and not a
// parallel implementation that only tests exercise.
func TestDescriptorHasExtendedACLUsesTheSystemLifecycle(t *testing.T) {
	t.Parallel()

	loader := systemDarwinSymbolLoader()
	if loader.open == nil || loader.symbol == nil ||
		loader.closeLib == nil || loader.register == nil {
		t.Fatal("systemDarwinSymbolLoader() left part of the dynamic-linker surface unset")
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "delegated.sqlite")
	if err := os.WriteFile(file, []byte("delegated"), 0o600); err != nil {
		t.Fatal(err)
	}
	fd, openErr := syscall.Open(file, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if openErr != nil {
		t.Fatalf("open %q: %v", file, openErr)
	}
	defer func() {
		if closeErr := syscall.Close(fd); closeErr != nil {
			t.Errorf("close %q: %v", file, closeErr)
		}
	}()

	direct, directErr := inspectWithDarwinACLLibrary(fd, loader, inspectDarwinDescriptorACL)
	if directErr != nil {
		t.Fatalf("inspectWithDarwinACLLibrary() error = %v; want nil", directErr)
	}
	delegated, delegatedErr := descriptorHasExtendedACL(fd)
	if delegatedErr != nil {
		t.Fatalf("descriptorHasExtendedACL() error = %v; want nil", delegatedErr)
	}
	if direct != delegated {
		t.Errorf("descriptorHasExtendedACL() = %t; want %t from the system lifecycle", delegated, direct)
	}
}

// TestDarwinACLCheckedConversions covers the narrowing the C ACL interface forces.
// Both conversions must refuse a value they cannot represent rather than wrap it:
// a truncated descriptor would address some unrelated open file, and a widened
// negative errno would compare equal to no known code and read as an unexplained
// failure.
func TestDarwinACLCheckedConversions(t *testing.T) {
	t.Parallel()

	t.Run("descriptor", func(t *testing.T) {
		t.Parallel()

		for _, fd := range []int{0, 3, math.MaxInt32} {
			got, err := darwinACLDescriptor(fd)
			if err != nil {
				t.Errorf("darwinACLDescriptor(%d) error = %v; want nil", fd, err)
			}
			if int(got) != fd {
				t.Errorf("darwinACLDescriptor(%d) = %d; want %d", fd, got, fd)
			}
		}
		for _, fd := range []int{-1, math.MaxInt32 + 1} {
			if _, err := darwinACLDescriptor(fd); err == nil ||
				!strings.Contains(err.Error(), "out of range") {
				t.Errorf("darwinACLDescriptor(%d) error = %v; want an out-of-range refusal", fd, err)
			}
		}
	})

	t.Run("errno", func(t *testing.T) {
		t.Parallel()

		for _, value := range []int32{0, int32(syscall.ENOENT), int32(syscall.EINVAL), math.MaxInt32} {
			got, err := darwinErrno(value)
			if err != nil {
				t.Errorf("darwinErrno(%d) error = %v; want nil", value, err)
			}
			if got != syscall.Errno(value) {
				t.Errorf("darwinErrno(%d) = %v; want %v", value, got, syscall.Errno(value))
			}
		}
		for _, value := range []int32{-1, math.MinInt32} {
			if _, err := darwinErrno(value); err == nil ||
				!strings.Contains(err.Error(), "out-of-range") {
				t.Errorf("darwinErrno(%d) error = %v; want an out-of-range refusal", value, err)
			}
		}
	})
}

// TestDescriptorHasExtendedACLDetectsRealNontrivialACL is the positive half of
// TestDescriptorHasExtendedACLBindsRealLibSystem: a file carrying an explicit ACE
// must be reported as nontrivial, so the check refuses a real ACL grant rather
// than only agreeing that ordinary files have none. The fixture is created with
// macOS's own chmod so the binding surface stays exactly what production uses.
func TestDescriptorHasExtendedACLDetectsRealNontrivialACL(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "granted.sqlite")
	if err := os.WriteFile(file, []byte("granted"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A fixture that cannot be created is a failure, not a skip: this is the only
	// test that proves the check refuses a real ACL grant, so silently passing
	// when the ACE was never applied would certify nothing.
	if output, err := exec.Command("/bin/chmod", "+a", "everyone deny delete", file).CombinedOutput(); err != nil {
		t.Fatalf("create Darwin ACL fixture for %q: %v: %s", file, err, output)
	}
	// Registered the moment the ACE exists, and never deferred. The fixture denies
	// delete to everyone, which is exactly the right that TempDir's RemoveAll needs,
	// so the ACE has to come off before that runs or the temp tree is stranded.
	//
	// t.Cleanup is what orders this correctly on both sides. Deferred calls in the
	// test body run when the body returns, before any cleanup, so the descriptor
	// below is already closed by the time chmod rewrites the file's ACL. And because
	// t.TempDir registered its removal first and cleanups run last-in-first-out,
	// this one runs ahead of it.
	t.Cleanup(func() { removeDarwinACLFixture(t, file) })

	fd, openErr := syscall.Open(file, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if openErr != nil {
		t.Fatalf("open %q: %v", file, openErr)
	}
	defer func() {
		if closeErr := syscall.Close(fd); closeErr != nil {
			t.Errorf("close %q: %v", file, closeErr)
		}
	}()

	hasACL, err := descriptorHasExtendedACL(fd)
	if err != nil {
		t.Fatalf("descriptorHasExtendedACL(%q) error = %v; want nil", file, err)
	}
	if !hasACL {
		t.Errorf("descriptorHasExtendedACL(%q) = false; want true for a file carrying an explicit ACE", file)
	}
}

// removeDarwinACLFixture strips every ACE from path using macOS's own chmod, the
// inverse of the `+a` that created the fixture.
//
// Failure is reported, never swallowed. `chmod -N` is what makes the fixture file
// deletable again, so a silent failure here would surface later as an unrelated
// TempDir removal error in some other test's output, with nothing pointing back at
// the ACL that caused it. Unexpected output is treated the same way: chmod is
// silent on success, so anything on stdout or stderr means the removal did not go
// as expected even when the exit status is zero.
func removeDarwinACLFixture(t *testing.T, path string) {
	t.Helper()

	output, err := exec.Command("/bin/chmod", "-N", path).CombinedOutput()
	if removalErr := darwinACLRemovalError(path, output, err); removalErr != nil {
		t.Error(removalErr)
	}
}

// darwinACLRemovalError classifies a `chmod -N` result. It is split out from the
// reporting so the classification is testable without macOS: the surrounding test
// only ever runs where /bin/chmod exists, but the rule that silent-and-successful
// is the sole accepted outcome should not depend on that to be checked.
func darwinACLRemovalError(path string, output []byte, err error) error {
	if err != nil {
		return fmt.Errorf("remove Darwin ACL fixture from %q: %w: %s", path, err, output)
	}
	if trimmed := strings.TrimSpace(string(output)); trimmed != "" {
		return fmt.Errorf("remove Darwin ACL fixture from %q: unexpected chmod output: %s", path, trimmed)
	}
	return nil
}

// TestDarwinACLRemovalError pins the cleanup's accept/reject rule. Only a silent,
// zero-status chmod counts as a removal; treating a warning-but-succeeded run as
// success would let a still-ACL'd fixture reach TempDir's RemoveAll unreported.
func TestDarwinACLRemovalError(t *testing.T) {
	t.Parallel()

	const path = "/tmp/fixture.sqlite"

	for _, test := range []struct {
		name    string
		output  string
		err     error
		wantErr []string
	}{
		{name: "silent success is the only accepted outcome"},
		{
			name:    "nonzero status reports the command output",
			output:  "chmod: Failed to clear ACL on file fixture.sqlite: Operation not permitted",
			err:     syscall.EPERM,
			wantErr: []string{path, "operation not permitted", "Failed to clear ACL"},
		},
		{
			name:    "output without a nonzero status is still a failure",
			output:  "chmod: fixture.sqlite: No ACL present",
			wantErr: []string{path, "unexpected chmod output", "No ACL present"},
		},
		{
			name:   "trailing newline alone is not output",
			output: "\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := darwinACLRemovalError(path, []byte(test.output), test.err)
			if len(test.wantErr) == 0 {
				if err != nil {
					t.Errorf("darwinACLRemovalError() = %v; want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("darwinACLRemovalError() = nil; want %v", test.wantErr)
			}
			for _, want := range test.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("darwinACLRemovalError() = %v; want it to mention %q", err, want)
				}
			}
		})
	}
}

func TestInspectDarwinDescriptorACL(t *testing.T) {
	t.Parallel()

	const (
		fd        = 123
		aclHandle = uintptr(456)
	)

	for _, test := range []struct {
		name      string
		configure func(*testing.T, *darwinACLBindings, *int32)
		wantACL   bool
		wantErr   string
		wantFree  int
	}{
		{
			name: "entry present",
			configure: func(t *testing.T, bindings *darwinACLBindings, _ *int32) {
				t.Helper()

				bindings.getFD = func(gotFD, aclType int32) uintptr {
					if gotFD != fd || aclType != darwinACLTypeExtended {
						t.Fatalf("acl_get_fd_np(%d, %d); want %d, %d", gotFD, aclType, fd, darwinACLTypeExtended)
					}
					return aclHandle
				}
				bindings.getEntry = func(acl uintptr, entryID int32, entry *uintptr) int32 {
					if acl != aclHandle || entryID != darwinACLFirstEntry || entry == nil {
						t.Fatalf("acl_get_entry(%d, %d, %p); want %d, %d, non-nil", acl, entryID, entry, aclHandle, darwinACLFirstEntry)
					}
					*entry = 789
					return 0
				}
			},
			wantACL:  true,
			wantFree: 1,
		},
		{
			name: "no stored ACL",
			configure: func(_ *testing.T, bindings *darwinACLBindings, errnoValue *int32) {
				bindings.getFD = func(int32, int32) uintptr {
					*errnoValue = int32(syscall.ENOENT)
					return 0
				}
			},
		},
		{
			name: "empty returned ACL",
			configure: func(_ *testing.T, bindings *darwinACLBindings, errnoValue *int32) {
				bindings.getFD = func(int32, int32) uintptr { return aclHandle }
				bindings.getEntry = func(uintptr, int32, *uintptr) int32 {
					*errnoValue = int32(syscall.EINVAL)
					return -1
				}
			},
			wantFree: 1,
		},
		{
			name: "get ACL failure",
			configure: func(_ *testing.T, bindings *darwinACLBindings, errnoValue *int32) {
				bindings.getFD = func(int32, int32) uintptr {
					*errnoValue = int32(syscall.EACCES)
					return 0
				}
			},
			wantErr: "permission denied",
		},
		{
			name: "get entry failure",
			configure: func(_ *testing.T, bindings *darwinACLBindings, errnoValue *int32) {
				bindings.getFD = func(int32, int32) uintptr { return aclHandle }
				bindings.getEntry = func(uintptr, int32, *uintptr) int32 {
					*errnoValue = int32(syscall.EIO)
					return -1
				}
			},
			wantErr:  "input/output error",
			wantFree: 1,
		},
		{
			name: "cleanup failure",
			configure: func(_ *testing.T, bindings *darwinACLBindings, errnoValue *int32) {
				bindings.getFD = func(int32, int32) uintptr { return aclHandle }
				bindings.getEntry = func(uintptr, int32, *uintptr) int32 {
					*errnoValue = int32(syscall.EINVAL)
					return -1
				}
				bindings.free = func(uintptr) int32 {
					*errnoValue = int32(syscall.EIO)
					return -1
				}
			},
			wantErr:  "free Darwin ACL",
			wantFree: 1,
		},
		{
			name: "missing errno storage",
			configure: func(_ *testing.T, bindings *darwinACLBindings, _ *int32) {
				bindings.errnoLocation = func() *int32 { return nil }
			},
			wantErr: "errno storage",
		},
		{
			name: "no ACL without errno",
			configure: func(_ *testing.T, bindings *darwinACLBindings, _ *int32) {
				bindings.getFD = func(int32, int32) uintptr { return 0 }
			},
			wantErr: "acl_get_fd_np returned no ACL without errno",
		},
		{
			name: "entry failure without errno",
			configure: func(_ *testing.T, bindings *darwinACLBindings, _ *int32) {
				bindings.getFD = func(int32, int32) uintptr { return aclHandle }
				bindings.getEntry = func(uintptr, int32, *uintptr) int32 { return -1 }
			},
			wantErr:  "acl_get_entry failed without errno",
			wantFree: 1,
		},
		{
			name: "unexpected entry result",
			configure: func(_ *testing.T, bindings *darwinACLBindings, _ *int32) {
				bindings.getFD = func(int32, int32) uintptr { return aclHandle }
				bindings.getEntry = func(uintptr, int32, *uintptr) int32 { return 7 }
			},
			wantErr:  "acl_get_entry returned unexpected result 7",
			wantFree: 1,
		},
		{
			name: "cleanup failure without errno",
			configure: func(_ *testing.T, bindings *darwinACLBindings, errnoValue *int32) {
				bindings.getFD = func(int32, int32) uintptr { return aclHandle }
				bindings.getEntry = func(uintptr, int32, *uintptr) int32 {
					*errnoValue = int32(syscall.EINVAL)
					return -1
				}
				bindings.free = func(uintptr) int32 { return -1 }
			},
			wantErr:  "acl_free returned -1 without errno",
			wantFree: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			errnoValue := int32(0)
			freeCalls := 0
			bindings := darwinACLBindings{
				getFD: func(int32, int32) uintptr { return aclHandle },
				getEntry: func(uintptr, int32, *uintptr) int32 {
					errnoValue = int32(syscall.EINVAL)
					return -1
				},
				free:          func(uintptr) int32 { return 0 },
				errnoLocation: func() *int32 { return &errnoValue },
			}
			test.configure(t, &bindings, &errnoValue)
			originalFree := bindings.free
			bindings.free = func(acl uintptr) int32 {
				freeCalls++
				return originalFree(acl)
			}

			hasACL, err := inspectDarwinDescriptorACL(fd, bindings)
			if hasACL != test.wantACL {
				t.Errorf("inspectDarwinDescriptorACL() hasACL = %t; want %t", hasACL, test.wantACL)
			}
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("inspectDarwinDescriptorACL() error = %v; want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("inspectDarwinDescriptorACL() error = %v; want %q", err, test.wantErr)
			}
			if freeCalls != test.wantFree {
				t.Errorf("acl_free calls = %d; want %d", freeCalls, test.wantFree)
			}
		})
	}
}
