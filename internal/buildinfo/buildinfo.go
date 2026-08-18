// Package buildinfo exposes static build metadata for the patchbay binary.
package buildinfo

// version is the current patchbay release identifier. It is bumped by hand as
// the project cuts releases; nothing derives from it at build time yet.
const version = "0.0.0-dev"

// Version returns the patchbay build version string.
func Version() string {
	return version
}
