//go:build darwin

package usage

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"syscall"

	"github.com/ebitengine/purego"
)

const (
	darwinACLTypeExtended = int32(0x00000100)
	darwinACLFirstEntry   = int32(0)
)

type darwinACLBindings struct {
	getFD         func(int32, int32) uintptr
	getEntry      func(uintptr, int32, *uintptr) int32
	free          func(uintptr) int32
	errnoLocation func() *int32
}

// descriptorHasExtendedACL binds libSystem, inspects fd, and releases the library
// again. The binding is deliberately scoped to the call rather than memoized in
// package state: path validation performs a bounded handful of ACL checks per
// database open, never a hot loop, so owning the handle for exactly as long as it
// is used buys strict open/use/close accounting and leaves no shared mutable
// state whose lifetime nothing can account for. libSystem is mapped for the
// lifetime of every macOS process, so the repeated Dlopen resolves against the
// already-loaded image and the matching Dlclose only decrements its count.
func descriptorHasExtendedACL(fd int) (bool, error) {
	return inspectWithDarwinACLLibrary(fd, systemDarwinSymbolLoader(), inspectDarwinDescriptorACL)
}

// darwinACLInspector is the inspection step of the library lifecycle. Injecting it
// alongside the loader makes the release path observable: a test can drive an
// inspection to success, to a returned error, or to a panic and still assert that
// libSystem was released exactly once.
type darwinACLInspector func(fd int, bindings darwinACLBindings) (bool, error)

// inspectWithDarwinACLLibrary owns the library for one inspection: bind, inspect,
// release. Every exit releases the handle exactly once, including a panic out of a
// purego trampoline, which is recovered and converted into a refusal rather than
// unwinding through path validation.
//
// A failed release forces the verdict to false. The alternative — reporting "no
// ACL" while a handle stayed open — would let path validation accept a database on
// the word of an inspection whose own cleanup did not complete.
func inspectWithDarwinACLLibrary(
	fd int,
	loader darwinSymbolLoader,
	inspect darwinACLInspector,
) (hasACL bool, result error) {
	library, bindings, bindErr := bindDarwinACLFunctions(loader)
	if bindErr != nil {
		return false, bindErr
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			hasACL = false
			result = fmt.Errorf("inspect Darwin descriptor ACL: %v", recovered)
		}
		if closeErr := closeDarwinACLLibrary(loader, library, darwinACLInspectionStage); closeErr != nil {
			hasACL = false
			result = errors.Join(result, closeErr)
		}
	}()
	return inspect(fd, bindings)
}

// darwinSymbolLoader is the dynamic-linker surface the ACL binding needs.
// Injecting it keeps the resolution, panic, and cleanup paths exercisable without
// CGO or a real libSystem.
type darwinSymbolLoader struct {
	open     func(path string, mode int) (uintptr, error)
	symbol   func(library uintptr, name string) (uintptr, error)
	closeLib func(library uintptr) error
	register func(target any, address uintptr)
}

func systemDarwinSymbolLoader() darwinSymbolLoader {
	return darwinSymbolLoader{
		open:     purego.Dlopen,
		symbol:   purego.Dlsym,
		closeLib: purego.Dlclose,
		register: purego.RegisterFunc,
	}
}

// bindDarwinACLFunctions opens libSystem and binds the ACL surface. On success it
// hands the still-open handle to the caller, which owns closing it; on every
// failure path it releases the handle itself, because the caller has no usable
// bindings to close it with.
func bindDarwinACLFunctions(loader darwinSymbolLoader) (uintptr, darwinACLBindings, error) {
	library, openErr := loader.open(
		"/usr/lib/libSystem.B.dylib",
		purego.RTLD_NOW|purego.RTLD_LOCAL,
	)
	if openErr != nil {
		return 0, darwinACLBindings{}, fmt.Errorf("open libSystem for ACL inspection: %w", openErr)
	}

	bindings, bindErr := registerDarwinACLSymbols(loader, library)
	if bindErr != nil {
		// The caller receives no usable bindings and no handle, so it has nothing
		// to close with. Drop the reference here rather than leaking a libSystem
		// count on every failed inspection.
		return 0, darwinACLBindings{}, errors.Join(
			bindErr,
			closeDarwinACLLibrary(loader, library, darwinACLBindingStage),
		)
	}
	return library, bindings, nil
}

// registerDarwinACLSymbols resolves and binds every ACL function, recovering the
// panic purego raises when a target does not describe a supported calling
// convention so the caller can still release the library.
func registerDarwinACLSymbols(
	loader darwinSymbolLoader,
	library uintptr,
) (bindings darwinACLBindings, result error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			bindings = darwinACLBindings{}
			result = fmt.Errorf("register Darwin ACL functions: %v", recovered)
		}
	}()

	for _, symbol := range []struct {
		name   string
		target any
	}{
		{name: "acl_get_fd_np", target: &bindings.getFD},
		{name: "acl_get_entry", target: &bindings.getEntry},
		{name: "acl_free", target: &bindings.free},
		{name: "__error", target: &bindings.errnoLocation},
	} {
		address, symbolErr := loader.symbol(library, symbol.name)
		if symbolErr != nil {
			return darwinACLBindings{}, fmt.Errorf("resolve %s: %w", symbol.name, symbolErr)
		}
		loader.register(symbol.target, address)
	}
	return bindings, nil
}

// Stages a libSystem release can happen at. The two paths release the handle at
// different points for different reasons, so the message names the stage instead
// of asserting a failure that may not have occurred.
const (
	darwinACLBindingStage    = "failed ACL binding"
	darwinACLInspectionStage = "Darwin ACL inspection"
)

func closeDarwinACLLibrary(loader darwinSymbolLoader, library uintptr, stage string) error {
	if closeErr := loader.closeLib(library); closeErr != nil {
		return fmt.Errorf("close libSystem after %s: %w", stage, closeErr)
	}
	return nil
}

func inspectDarwinDescriptorACL(
	fd int,
	bindings darwinACLBindings,
) (hasACL bool, result error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	descriptor, descriptorErr := darwinACLDescriptor(fd)
	if descriptorErr != nil {
		return false, descriptorErr
	}
	errnoLocation := bindings.errnoLocation()
	if errnoLocation == nil {
		return false, errors.New("inspect Darwin descriptor ACL: libSystem returned nil errno storage")
	}
	*errnoLocation = 0
	acl := bindings.getFD(descriptor, darwinACLTypeExtended)
	if acl == 0 {
		return false, interpretDarwinMissingACL(*errnoLocation)
	}
	defer func() {
		*errnoLocation = 0
		freeResult := bindings.free(acl)
		if freeErr := darwinACLFreeError(freeResult, *errnoLocation); freeErr != nil {
			result = errors.Join(result, freeErr)
		}
	}()

	*errnoLocation = 0
	var entry uintptr
	entryResult := bindings.getEntry(acl, darwinACLFirstEntry, &entry)
	return interpretDarwinACLEntry(entryResult, *errnoLocation)
}

// interpretDarwinMissingACL decides what a nil acl_get_fd_np result means. The
// descriptor carries no extended ACL either way, so the verdict is always "no
// ACL"; only whether that verdict is trustworthy varies. ENOENT is the ordinary
// answer, and anything else is an inspection failure, which is fatal to path
// validation rather than treated as an absent ACL.
func interpretDarwinMissingACL(errnoValue int32) error {
	aclErrno, errnoErr := darwinErrno(errnoValue)
	if errnoErr != nil {
		return errnoErr
	}
	switch aclErrno {
	case syscall.ENOENT:
		return nil
	case 0:
		return errors.New("acl_get_fd_np returned no ACL without errno")
	default:
		return fmt.Errorf("get Darwin descriptor ACL: %w", aclErrno)
	}
}

// interpretDarwinACLEntry reads the first-entry probe. A returned entry means the
// ACL carries at least one ACE and is therefore nontrivial; EINVAL means the ACL
// exists but is empty.
func interpretDarwinACLEntry(entryResult, errnoValue int32) (bool, error) {
	entryErrno, errnoErr := darwinErrno(errnoValue)
	if errnoErr != nil {
		return false, errnoErr
	}
	if entryResult == 0 {
		return true, nil
	}
	if entryResult != -1 {
		return false, fmt.Errorf("acl_get_entry returned unexpected result %d", entryResult)
	}
	switch entryErrno {
	case syscall.EINVAL:
		return false, nil
	case 0:
		return false, errors.New("acl_get_entry failed without errno")
	default:
		return false, fmt.Errorf("inspect Darwin descriptor ACL entries: %w", entryErrno)
	}
}

// darwinACLFreeError reports a failed acl_free. Leaking the ACL would be silent,
// so the failure is joined onto whatever verdict the inspection reached.
func darwinACLFreeError(freeResult, errnoValue int32) error {
	if freeResult == 0 {
		return nil
	}
	freeErrno, errnoErr := darwinErrno(errnoValue)
	if errnoErr != nil {
		return errnoErr
	}
	if freeErrno == 0 {
		return fmt.Errorf("free Darwin ACL: acl_free returned %d without errno", freeResult)
	}
	return fmt.Errorf("free Darwin ACL: %w", freeErrno)
}

// darwinACLDescriptor narrows a Go file descriptor to the int32 the C ACL
// functions accept. A descriptor that cannot round-trip is refused rather than
// truncated into some other process-visible descriptor.
func darwinACLDescriptor(fd int) (int32, error) {
	if fd < 0 || fd > math.MaxInt32 {
		return 0, fmt.Errorf("file descriptor %d is out of range for the Darwin ACL interface", fd)
	}
	return int32(fd), nil
}

// darwinErrno converts a value read from libSystem's errno storage. errno codes
// are non-negative, so a negative value means the storage did not hold one and
// the inspection must fail closed rather than widen it into a huge Errno.
func darwinErrno(value int32) (syscall.Errno, error) {
	if value < 0 {
		return 0, fmt.Errorf("read Darwin errno storage: out-of-range value %d", value)
	}
	return syscall.Errno(value), nil
}
