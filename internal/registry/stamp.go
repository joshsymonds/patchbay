package registry

import (
	"os"
	"path/filepath"
	"time"
)

// fileStamp is the cheap identity of the registry file: enough to decide
// whether a cached parse is still current, without re-reading the file.
//
// Both halves matter. home-manager may write the registry in place, which moves
// the modification time, or point a symlink at a fresh /nix/store path, which
// leaves the modification time of the new target arbitrary — a store path's
// timestamp is normalised, so two generations can share one. Tracking the
// resolved target as well catches the swap either way.
type fileStamp struct {
	exists  bool
	modTime time.Time
	size    int64
	target  string
}

// statFile stamps path without reading it. A file that cannot be stat'ed —
// missing, dangling symlink, unreadable directory — stamps as absent, which is
// a state like any other and reloads the same way.
func statFile(path string) fileStamp {
	var target string
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}

	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{target: target}
	}

	return fileStamp{
		exists:  true,
		modTime: info.ModTime(),
		size:    info.Size(),
		target:  target,
	}
}

// equal reports whether two stamps describe the same file contents as far as
// the filesystem is willing to say.
func (s fileStamp) equal(other fileStamp) bool {
	return s.exists == other.exists &&
		s.size == other.size &&
		s.target == other.target &&
		s.modTime.Equal(other.modTime)
}
