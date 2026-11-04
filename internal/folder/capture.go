package folder

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"
)

// capture holds a pinned parent, not a copy of the inode. The coordinator owns
// serialization (scanMu -> saveMu -> mu), root/parent checks, post-capture hash
// validation, intent persistence and directory flushes. Close never deletes data.
// Persist Original and Recovery in the transaction intent before take.
type capture struct {
	parent             *os.File
	Original, Recovery string // relative Disk paths; may be NFD
	ops                platformFSOps
	identity           os.FileInfo // dev/ino from the descriptor opened by take
}

func captureToken() string {
	var b [12]byte
	// crypto/rand.Read cannot fail on the supported Go runtime.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// prepareCapture does not mutate the tree, so its names can be journaled first.
func (r *rootFS) prepareCapture(original string) (*capture, error) {
	return r.openCapture(original, path.Join(path.Dir(original), tmpPrefix+"capture-"+captureToken()))
}

// openCapture reopens an intent's names during recovery without following links.
func (r *rootFS) openCapture(original, recovery string) (*capture, error) {
	if !diskName(original) || !diskName(recovery) || path.Dir(original) != path.Dir(recovery) ||
		!strings.HasPrefix(path.Base(recovery), tmpPrefix+"capture-") || original == recovery {
		return nil, fmt.Errorf("invalid capture paths")
	}
	p, _, err := r.parent(original, false)
	if err != nil {
		return nil, err
	}
	return &capture{parent: p, Original: original, Recovery: recovery, ops: platformOps()}, nil
}

func (c *capture) Close() error { return c.parent.Close() }

func (c *capture) take() error {
	f, err := openRegularAt(c.parent, path.Base(c.Original), unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err = c.ops.renameNoReplace(c.parent, path.Base(c.Original), c.parent, path.Base(c.Recovery)); err != nil {
		return err
	}
	c.identity = info
	return nil
}

// restore preserves a target created after capture, promoting the captured inode
// to a visible conflict instead. On any other error the recovery entry stays put.
func (c *capture) restore(peer, hash string) (string, error) {
	err := c.ops.renameNoReplace(c.parent, path.Base(c.Recovery), c.parent, path.Base(c.Original))
	if err == nil {
		return c.Original, nil
	}
	if !errors.Is(err, unix.EEXIST) {
		return "", err
	}
	return c.promote(peer, hash)
}

// discard is called only after the index is durably committed. The coordinator
// must flush the parent afterwards, then remove and flush the transaction intent.
func (c *capture) discard() error {
	if c.identity == nil {
		return fmt.Errorf("capture identity unavailable")
	}
	f, err := openRegularAt(c.parent, path.Base(c.Recovery), unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	// SameFile compares device and inode on the supported Unix platforms.
	if !os.SameFile(c.identity, info) {
		return fmt.Errorf("capture inode changed: %s", c.Recovery)
	}
	return unix.Unlinkat(int(c.parent.Fd()), path.Base(c.Recovery), 0)
}

func clipCapture(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func captureLabel(s string) string {
	// IDs and version hashes are labels, never path components from a peer.
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return clipCapture(b.String(), 12)
}

// conflictLeaf fits NAME_MAX=255 on supported APFS/ext4 filesystems. Bound the
// extension as well as the base; truncation never splits UTF-8 and output is NFC.
func conflictLeaf(original, peer, hash, token string) string {
	leaf := norm.NFC.String(path.Base(original))
	ext := clipCapture(path.Ext(leaf), 32)
	base := strings.TrimSuffix(leaf, path.Ext(leaf))
	suffix := ".sync-conflict-" + captureLabel(peer) + "-" + captureLabel(hash) + "-" + token + ext
	return norm.NFC.String(clipCapture(base, 255-len(suffix)) + suffix)
}

func (c *capture) promote(peer, hash string) (string, error) {
	return c.promoteWithToken(peer, hash, captureToken)
}

func (c *capture) promoteWithToken(peer, hash string, token func() string) (string, error) {
	for {
		leaf := conflictLeaf(c.Original, peer, hash, token())
		err := c.ops.renameNoReplace(c.parent, path.Base(c.Recovery), c.parent, leaf)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", err
		}
		return path.Join(path.Dir(c.Original), leaf), nil
	}
}
