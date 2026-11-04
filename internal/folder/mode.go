package folder

import "os"

// incomingMode keeps the local rwx policy for an existing file and applies
// only the peer's executable bit. New files use the sharedir defaults.
func incomingMode(local os.FileMode, exists, exec bool) os.FileMode {
	if !exists {
		if exec {
			return 0o755
		}
		return 0o644
	}
	mode := local.Perm() & 0o777
	if exec {
		mode |= (mode & 0o444) >> 2
	} else {
		mode &^= 0o111
	}
	return mode
}
