package folder

import (
	"fmt"
	"path"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// groupIntent is the local JSON recovery record for G2–G5. It is distinct
// from the schema=3 Commit frame and from the legacy single-file intent.
// All paths are local rooted paths, never peer-supplied Disk values.
type groupIntent struct {
	Schema                                          uint32
	StoreID, TxID                                   [16]byte
	BaseGeneration, Generation, ConfirmedGeneration uint64
	Binding                                         rootBinding
	Clock                                           uint64
	Entries                                         []groupIntentEntry
	// Parents created during staging/mutation, retained for directory flushing.
	CreatedParents []string
}

type groupIntentEntry struct {
	Target, Tmp, Capture, Conflict string
	Before, After                  *FileInfo
	Identity, IncomingIdentity     captureIdentity
}

// validate is pure: callers must still guardRoot and recheck decisions under
// scanMu/saveMu before G2. After G2, cancellation cannot abandon this record.
func (g groupIntent) validate(binding rootBinding, confirmedGeneration uint64) error {
	invalid := func(reason string) error { return fmt.Errorf("invalid group intent: %s", reason) }
	if g.Schema != storeSchema || g.StoreID == ([16]byte{}) || g.TxID == ([16]byte{}) ||
		g.BaseGeneration == ^uint64(0) || g.Generation != g.BaseGeneration+1 || len(g.Entries) == 0 || len(g.Entries) > 64 {
		return invalid("header")
	}
	if g.Binding != binding || g.ConfirmedGeneration != confirmedGeneration || g.Binding.Path == "" || g.Binding.Inode == 0 {
		return invalid("binding or confirmed generation mismatch")
	}
	// NFC comparison also catches two spellings of one local disk name. Reserve
	// auxiliary names too: recovery must never promote over another operation.
	reserved := make(map[string]bool)
	reserve := func(name string) error {
		if !diskName(name) {
			return invalid("path")
		}
		name = norm.NFC.String(name)
		for other := range reserved {
			if name == other || strings.HasPrefix(name, other+"/") || strings.HasPrefix(other, name+"/") {
				return invalid("duplicate or dependent reserved path")
			}
		}
		reserved[name] = true
		return nil
	}
	for _, e := range g.Entries {
		j := journalIntent{TxID: "group", Generation: g.Generation, Binding: g.Binding,
			Target: e.Target, Tmp: e.Tmp, Capture: e.Capture, Before: e.Before, After: e.After, Identity: e.Identity}
		if err := j.validate(); err != nil {
			return err
		}
		for _, fi := range []*FileInfo{e.Before, e.After} {
			if fi == nil {
				continue
			}
			if fi.Size < 0 {
				return invalid("negative size")
			}
			for peer, counter := range fi.Version {
				if peer == "" || counter == 0 {
					return invalid("version")
				}
			}
		}
		if e.Before != nil && e.After != nil && e.Before.Name != e.After.Name {
			return invalid("record name mismatch")
		}
		if e.Tmp != "" && (e.IncomingIdentity.Inode == 0 || e.After == nil || e.After.Deleted) {
			return invalid("incoming")
		}
		if e.Conflict != "" && (!validName(norm.NFC.String(e.Conflict)) || path.Dir(e.Conflict) != path.Dir(e.Target) || strings.HasPrefix(path.Base(e.Conflict), tmpPrefix)) {
			return invalid("conflict")
		}
		for _, name := range []string{e.Target, e.Tmp, e.Capture, e.Conflict} {
			if name != "" {
				if err := reserve(name); err != nil {
					return err
				}
			}
		}
	}
	parents := make(map[string]bool)
	for _, name := range g.CreatedParents {
		if !diskName(name) || parents[norm.NFC.String(name)] || reserved[norm.NFC.String(name)] {
			return invalid("created parent")
		}
		parents[norm.NFC.String(name)] = true
	}
	return nil
}
