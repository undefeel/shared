package folder

import (
	"path"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// validName accepts only canonical peer names: clean relative slash paths in NFC.
func validName(name string) bool {
	return utf8.ValidString(name) && name != "" && name != "." && !path.IsAbs(name) && path.Clean(name) == name &&
		!strings.ContainsAny(name, "\\\x00") && name != ".." && !strings.HasPrefix(name, "../") && norm.NFC.IsNormalString(name)
}

// validDisk accepts a local relative path. Disk is persisted locally, may be
// NFD on filesystems that enumerate it so, and never comes from the peer.
func validDisk(name, disk string) bool {
	if disk == "" {
		return validName(name)
	}
	return validName(name) && utf8.ValidString(disk) && diskName(disk)
}
