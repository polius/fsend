package transfer

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// partialSuffix is appended to a destination filename while its bytes are in
// flight. Renamed away atomically once the BLAKE3 root verifies.
const partialSuffix = ".fsend-partial"

// disposition is the receiver's local verdict for one listing entry.
type disposition uint8

const (
	dispNew       disposition = iota // absent → create/send
	dispIdentical                    // present and the same → skip
	dispDiffers                      // present, content/target differs
	dispConflict                     // present with a different kind (file↔dir↔symlink)
	dispResume                       // partial present → resume
	dispVerify                       // --checksum: present at matching size → hash to decide
)

// entryPlan is one classified entry: its disposition, resolved target, and
// the wire Decision the sender receives (filled after consent).
type entryPlan struct {
	entry        wire.ListingEntry
	target       string
	disp         disposition
	resumeOffset uint64
	imohash      [ImohashSize]byte
}

// isConflict reports a destructive disagreement requiring --overwrite.
func (p *entryPlan) needsConsent() bool { return p.disp == dispDiffers || p.disp == dispConflict }

// classifySummary is the breakdown shown in the accept prompt.
type classifySummary struct {
	Total       int
	NewItems    int
	Identical   int
	Differing   int // dispDiffers + dispConflict
	BytesToRecv uint64
}

// Conflict describes one entry that disagrees with what's on disk, for the
// consolidated overwrite prompt.
type Conflict struct {
	RelativePath string
	Kind         string // "differs" | "file vs folder" | "folder vs file" | "symlink"
	LocalSize    int64
	IncomingSize uint64
}

// classify resolves every listing entry against targetDir. With checksum set,
// a regular file that exists at a matching size is marked dispVerify (decided
// later by a content hash) instead of by mtime. Returns the per-entry plans
// (in listing order) or an error if any path is unsafe.
func classify(entries []wire.ListingEntry, targetDir string, checksum bool) ([]entryPlan, error) {
	plans := make([]entryPlan, 0, len(entries))
	for _, e := range entries {
		rel, err := SanitizeRelativePath(e.RelativePath)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", fserrors.ErrPathTraversal, err)
		}
		target := filepath.Join(targetDir, rel)
		// Defense-in-depth: target must stay under targetDir.
		if abs, err := filepath.Abs(target); err == nil {
			if absDir, err2 := filepath.Abs(targetDir); err2 == nil && !pathIsUnder(abs, absDir) {
				return nil, fserrors.ErrPathTraversal
			}
		}
		plans = append(plans, classifyOne(e, target, targetDir, checksum))
	}
	return plans, nil
}

// ancestorBlocked reports whether target can't be placed because the nearest
// existing ancestor directory is actually a non-directory (e.g. a file sits
// where a parent folder needs to be). Walks up to targetDir. Portable: it
// inspects file types rather than relying on Lstat's error code, which differs
// across platforms (Unix ENOTDIR vs Windows ERROR_PATH_NOT_FOUND, the latter
// reported as "not exist").
func ancestorBlocked(target, targetDir string) bool {
	rootAbs, err := filepath.Abs(targetDir)
	if err != nil {
		return false
	}
	dir := filepath.Dir(target)
	for {
		absDir, err := filepath.Abs(dir)
		if err != nil || absDir == rootAbs {
			return false // reached the (directory) transfer root
		}
		if st, err := os.Lstat(dir); err == nil {
			return !st.IsDir() // first existing ancestor decides
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false // hit the filesystem root
		}
		dir = parent
	}
}

func classifyOne(e wire.ListingEntry, target, targetDir string, checksum bool) entryPlan {
	p := entryPlan{entry: e, target: target, disp: dispNew}
	st, statErr := os.Lstat(target)
	exists := statErr == nil

	switch e.Type {
	case wire.EntryDir:
		switch {
		case !exists:
			p.disp = dispNew
		case st.IsDir():
			p.disp = dispIdentical
		default:
			p.disp = dispConflict
		}
		return p

	case wire.EntrySymlink:
		switch {
		case !exists:
			p.disp = dispNew
		case st.Mode()&os.ModeSymlink != 0:
			if tgt, err := os.Readlink(target); err == nil && tgt == e.SymlinkTarget {
				p.disp = dispIdentical
			} else {
				p.disp = dispDiffers
			}
		default:
			p.disp = dispConflict
		}
		return p

	default: // EntryFile
		// Precedence: identical/verify target → resume → differ/conflict → new.
		if exists && st.Mode().IsRegular() && uint64(st.Size()) == e.Size {
			if checksum {
				// Same size — defer the skip/differ verdict to a content hash.
				p.disp = dispVerify
				return p
			}
			if st.ModTime().Unix() == e.ModTimeSec {
				p.disp = dispIdentical
				_ = os.Remove(target + partialSuffix) // drop any stale partial
				return p
			}
		}
		if off, imo, ok := resumeCandidate(target+partialSuffix, e.Size); ok {
			p.disp = dispResume
			p.resumeOffset = off
			p.imohash = imo
			return p
		}
		switch {
		case exists && (st.IsDir() || st.Mode()&os.ModeSymlink != 0):
			p.disp = dispConflict
		case exists:
			p.disp = dispDiffers
		case ancestorBlocked(target, targetDir):
			// A file sits where one of this file's parent folders needs to be;
			// it can't be placed until that conflict is resolved.
			p.disp = dispConflict
		default:
			p.disp = dispNew
		}
		return p
	}
}

// resumeCandidate reports whether a usable partial sits at path for a source
// of size total: a regular file whose chunk-aligned prefix is non-empty and
// short of total. Returns the aligned offset and its imohash fingerprint.
func resumeCandidate(path string, total uint64) (uint64, [ImohashSize]byte, bool) {
	var zero [ImohashSize]byte
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return 0, zero, false
	}
	existing := uint64(st.Size())
	aligned := (existing / wire.MaxChunkSize) * wire.MaxChunkSize
	if aligned == 0 || aligned >= total {
		if existing >= total {
			_ = os.Remove(path) // stale/oversized
		}
		return 0, zero, false
	}
	imo, err := PrefixImohash(path, int64(aligned))
	if err != nil {
		return 0, zero, false
	}
	return aligned, imo, true
}

// summarize tallies the plans for the accept prompt.
func summarize(plans []entryPlan) classifySummary {
	s := classifySummary{Total: len(plans)}
	for _, p := range plans {
		switch p.disp {
		case dispNew:
			s.NewItems++
			s.BytesToRecv += p.entry.Size
		case dispIdentical:
			s.Identical++
		case dispResume:
			s.NewItems++
			if p.entry.Size > p.resumeOffset {
				s.BytesToRecv += p.entry.Size - p.resumeOffset
			}
		case dispDiffers, dispConflict:
			s.Differing++
		}
	}
	return s
}

// conflicts extracts the consent-needing entries for the overwrite prompt.
func conflicts(plans []entryPlan) []Conflict {
	var out []Conflict
	for _, p := range plans {
		if !p.needsConsent() {
			continue
		}
		c := Conflict{RelativePath: p.entry.RelativePath, IncomingSize: p.entry.Size, Kind: "differs"}
		if st, err := os.Lstat(p.target); err == nil {
			c.LocalSize = st.Size()
			if p.disp == dispConflict {
				switch {
				case st.IsDir():
					c.Kind = "folder vs file"
				case st.Mode()&os.ModeSymlink != 0:
					c.Kind = "symlink"
				default:
					c.Kind = "file vs folder"
				}
			}
		}
		out = append(out, c)
	}
	return out
}
