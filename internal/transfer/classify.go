package transfer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// disposition is the receiver's local verdict for one listing entry.
type disposition uint8

const (
	dispNew       disposition = iota // absent → create/send
	dispIdentical                    // present and the same → skip
	dispDiffers                      // present, content/target differs
	dispConflict                     // present with a different kind (file↔dir↔symlink)
	dispResume                       // partial present → resume
	dispVerify                       // --checksum: present at matching size → hash to decide
	dispRefused                      // can't be placed safely (name collision, reserved name); skipped and reported
)

// entryPlan is one classified entry: its disposition, resolved target, and
// the wire Decision the sender receives (filled after consent).
type entryPlan struct {
	entry  wire.ListingEntry
	rel    string // OS-native path under the target dir (renamed under keep-both)
	target string
	disp   disposition

	resumeOffset uint64
	prefixBlocks [][32]byte // block hashes of the resume prefix
	partial      string     // resume partial path

	// sha256 holds the hex digest of the content written for this entry,
	// set when bytes actually landed and a manifest was requested.
	sha256 string
	// blockedAncestor marks a target reachable only by traversing a
	// receiver-side symlink in its path. Such an entry is kept untouched (never
	// sent, materialized, or chmod'd) even under --overwrite, so a write or
	// mode change can't escape the receive tree through the link.
	blockedAncestor bool
	// nonEmptyDir: a folder with contents sits where this entry goes. fsend
	// never deletes it, even under --overwrite.
	nonEmptyDir bool
	renamed     bool   // keep-both: saved under a fresh name
	skipReason  string // dispRefused: why, for the user
}

// needsConsent reports a destructive disagreement requiring --overwrite.
func (p *entryPlan) needsConsent() bool { return p.disp == dispDiffers || p.disp == dispConflict }

// classifySummary is the breakdown shown in the accept prompt.
type classifySummary struct {
	Total       int
	NewItems    int
	Identical   int
	Differing   int // dispDiffers + dispConflict
	Refused     int // entries that can't be placed (name collisions, reserved names)
	BytesToRecv uint64
	// OfferedBytes is the size of the whole payload (every entry), matching
	// the sender's total. The headline shows this so both sides agree on one
	// number; BytesToRecv is what the progress bar fills to.
	OfferedBytes uint64
	// DifferingBytes is the size of the differing/conflicting entries — bytes
	// that move only if the user consents to replace or keep both. The CLI
	// adds these to the bar total on consent so it doesn't read past 100%.
	DifferingBytes uint64
	Files          []SummaryEntry // per-file rows for the pre-transfer preview
	// Conflicts lists the entries that already exist and differ, so the
	// accept prompt can ask what to do with them in the same question.
	Conflicts []Conflict
}

// SummaryEntry is one regular file (or symlink) the receiver is offered, with
// the per-entry status the preview annotates rows with.
type SummaryEntry struct {
	Index         uint32 // wire file index, keys the progress callbacks
	RelativePath  string
	Size          uint64
	Status        string // "new" | "identical" | "differs" | "resume" | "refused"
	Type          wire.EntryType
	SymlinkTarget string // EntrySymlink only
}

// Conflict describes one entry that disagrees with what's on disk, for the
// consolidated overwrite prompt.
type Conflict struct {
	RelativePath string
	Kind         string // "differs" | "file vs folder" | "folder vs file" | "symlink"
	LocalSize    int64
	LocalModTime time.Time // local copy's mtime; the tiebreaker when sizes read equal
	IncomingSize uint64
	// NonEmptyDir: a folder with contents is in the way. Replace keeps it
	// (fsend never deletes a folder's contents); keep both still works.
	NonEmptyDir bool
}

// classifier resolves listing entries against the receive directory.
type classifier struct {
	targetDir string
	checksum  bool
	sem       fsSemantics
	ps        *partialStore
	progress  func(done, total uint64) // resume-prefix hashing
}

// classify resolves every listing entry against targetDir with the probed
// filesystem semantics. Kept as the simple entry point for tests.
func classify(entries []wire.ListingEntry, targetDir string, checksum bool) ([]entryPlan, error) {
	c := &classifier{targetDir: targetDir, checksum: checksum, sem: probeFS(targetDir), ps: newPartialStore(targetDir)}
	return c.run(entries)
}

// run returns the per-entry plans (in listing order) or an error if any path
// is unsafe or the listing names one path twice.
func (c *classifier) run(entries []wire.ListingEntry) ([]entryPlan, error) {
	plans := make([]entryPlan, 0, len(entries))
	exact := make(map[string]struct{}, len(entries))
	keys := make(map[string]string, len(entries)) // fs identity → first path
	absDir, err := filepath.Abs(c.targetDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		// Reject peer symlinks: the honest sender dereferences them and never
		// emits one (walk.go), so a symlink on the wire is a malicious sender
		// planting a link that a later write traverses to escape targetDir —
		// which the lexical path checks here can't see through.
		if e.Type == wire.EntrySymlink {
			return nil, fmt.Errorf("%w: peer sent a symlink (%q); fsend does not accept symlink entries", fserrors.ErrPathTraversal, e.RelativePath)
		}
		rel, err := SanitizeRelativePath(e.RelativePath)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", fserrors.ErrPathTraversal, err)
		}
		target := filepath.Join(c.targetDir, rel)
		// Defense-in-depth: target must stay under targetDir.
		if abs, err := filepath.Abs(target); err != nil || !pathIsUnder(abs, absDir) {
			return nil, fserrors.ErrPathTraversal
		}
		slash := filepath.ToSlash(rel)
		if _, dup := exact[slash]; dup {
			return nil, fmt.Errorf("%w: duplicate path %q", fserrors.ErrProtocolError, e.RelativePath)
		}
		exact[slash] = struct{}{}

		key := c.sem.nameKey(slash)
		first, collides := keys[key]
		if !collides {
			keys[key] = slash
		}
		switch {
		case isReserved(rel):
			plans = append(plans, entryPlan{entry: e, rel: rel, target: target, disp: dispRefused,
				skipReason: "the name " + stateDirName + " is reserved for fsend's partial downloads"})
			continue
		case collides:
			// Two spellings that this filesystem treats as one name (case, or
			// NFC/NFD): writing both would silently overwrite the first.
			plans = append(plans, entryPlan{entry: e, rel: rel, target: target, disp: dispRefused,
				skipReason: fmt.Sprintf("same name as %q on this filesystem", first)})
			continue
		}
		p := c.classifyOne(e, rel, target)
		p.blockedAncestor = symlinkAncestor(target, c.targetDir)
		plans = append(plans, p)
	}
	if err := c.hashResumePrefixes(plans); err != nil {
		return nil, err
	}
	return plans, nil
}

// hashResumePrefixes computes the block hashes of every resume partial's
// prefix, several files at a time, so the sender can verify them. A partial
// that can't be read is dropped and its file sent fresh.
func (c *classifier) hashResumePrefixes(plans []entryPlan) error {
	var total uint64
	var idx []int
	for i := range plans {
		if plans[i].disp == dispResume {
			idx = append(idx, i)
			total += plans[i].resumeOffset
		}
	}
	if len(idx) == 0 {
		return nil
	}
	prog := newProgressMeter(total, c.progress)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, i := range idx {
		wg.Add(1)
		sem <- struct{}{}
		go func(p *entryPlan) {
			defer func() { <-sem; wg.Done() }()
			blocks, err := hashPartialPrefix(p.partial, int64(p.resumeOffset), prog.add)
			if err != nil {
				c.ps.remove(p.rel)
				p.disp, p.resumeOffset, p.partial = dispNew, 0, ""
				return
			}
			p.prefixBlocks = blocks
		}(&plans[i])
	}
	wg.Wait()
	return nil
}

func hashPartialPrefix(path string, n int64, progress func(int64)) ([][32]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var last int64
	return hashBlocks(f, n, func(done int64) {
		progress(done - last)
		last = done
	})
}

// symlinkAncestor reports whether any path component of target between
// targetDir (exclusive) and target (exclusive) is a symlink. Lstat on the full
// target only inspects the leaf — an intermediate link is silently followed —
// so each ancestor is Lstat'd in turn, where it is itself the leaf. A
// receiver-planted link on the path would let a later write or chmod traverse
// out of the receive tree; callers must refuse to act on such a target.
func symlinkAncestor(target, targetDir string) bool {
	rootAbs, err := filepath.Abs(targetDir)
	if err != nil {
		return false
	}
	cur, err := filepath.Abs(filepath.Dir(target))
	if err != nil {
		return false
	}
	for cur != rootAbs {
		if st, err := os.Lstat(cur); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break // reached the filesystem root without meeting targetDir
		}
		cur = parent
	}
	return false
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

// dirHasEntries reports whether dir contains anything.
func dirHasEntries(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return true // unknown: treat as non-empty, never delete it
	}
	defer func() { _ = f.Close() }()
	names, _ := f.Readdirnames(1)
	return len(names) > 0
}

func (c *classifier) classifyOne(e wire.ListingEntry, rel, target string) entryPlan {
	p := entryPlan{entry: e, rel: rel, target: target, disp: dispNew}
	st, statErr := os.Lstat(target)
	exists := statErr == nil

	switch e.Type {
	case wire.EntryDir:
		switch {
		case !exists:
			if ancestorBlocked(target, c.targetDir) {
				p.disp = dispConflict
			}
		case st.IsDir():
			p.disp = dispIdentical
		default:
			p.disp = dispConflict
		}
		return p

	// No EntrySymlink case: classify() rejects peer symlinks outright before
	// reaching here (a security boundary — the sender dereferences symlinks
	// and never emits one).

	default: // EntryFile
		// Precedence: identical/verify target → resume → differ/conflict → new.
		if exists && st.Mode().IsRegular() && uint64(st.Size()) == e.Size {
			if c.checksum {
				// Same size — defer the skip/differ verdict to a content hash.
				p.disp = dispVerify
				return p
			}
			if c.sem.sameMtime(st.ModTime(), e.ModTimeSec) {
				p.disp = dispIdentical
				c.ps.remove(rel) // drop any stale partial
				return p
			}
		}
		// Resume only when the target is absent. An existing non-identical
		// target is the user's file — resuming would rename over it with no
		// consent, so fall through to differs/conflict (an approved
		// overwrite re-sends fresh, truncating the stale partial).
		if !exists {
			c.resumeCandidate(&p)
			if p.disp == dispResume {
				return p
			}
		}
		switch {
		case exists && st.IsDir():
			p.disp = dispConflict
			p.nonEmptyDir = dirHasEntries(target)
		case exists && st.Mode()&os.ModeSymlink != 0:
			p.disp = dispConflict
		case exists:
			p.disp = dispDiffers
		case ancestorBlocked(target, c.targetDir):
			// A file sits where one of this file's parent folders needs to be;
			// it can't be placed until that conflict is resolved.
			p.disp = dispConflict
		default:
			p.disp = dispNew
		}
		return p
	}
}

// resumeCandidate marks p for resume when a partial written from this exact
// source version (per its sidecar) holds at least one whole block. The
// offset stays below the size so the final block always crosses the wire
// and carries the EOF root.
func (c *classifier) resumeCandidate(p *entryPlan) {
	if p.entry.Size == 0 {
		return
	}
	path, size, ok := c.ps.lookup(p.rel, p.entry)
	if !ok {
		return
	}
	limit := min(uint64(size), p.entry.Size-1)
	aligned := limit / hashBlockSize * hashBlockSize
	if aligned == 0 {
		return
	}
	p.disp = dispResume
	p.resumeOffset = aligned
	p.partial = path
}

// summarize tallies the plans for the accept prompt.
func summarize(plans []entryPlan) classifySummary {
	s := classifySummary{Total: len(plans)}
	for _, p := range plans {
		s.OfferedBytes += p.entry.Size
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
		case dispRefused:
			if p.entry.Type != wire.EntryDir {
				s.Refused++
			}
		case dispDiffers, dispConflict:
			// A directory isn't a user-facing "file"; excluding it keeps the
			// "N differ" header and the --overwrite warning agreeing with the
			// per-file row count and the sender's kept tally.
			if p.entry.Type != wire.EntryDir {
				s.Differing++
				s.DifferingBytes += p.entry.Size
			}
		}
		// Directories aren't files the user thinks about; mirror CountFiles
		// so the preview's row count matches the headline's file count.
		if p.entry.Type != wire.EntryDir {
			s.Files = append(s.Files, SummaryEntry{
				Index:         p.entry.Index,
				RelativePath:  p.entry.RelativePath,
				Size:          p.entry.Size,
				Status:        dispStatus(p.disp),
				Type:          p.entry.Type,
				SymlinkTarget: p.entry.SymlinkTarget,
			})
		}
	}
	s.Conflicts = conflicts(plans)
	return s
}

// dispStatus maps a disposition to the preview's status vocabulary. Verify
// (same-size, undecided) is treated as incoming until the content hash rules.
func dispStatus(d disposition) string {
	switch d {
	case dispIdentical:
		return "identical"
	case dispDiffers, dispConflict:
		return "differs"
	case dispResume:
		return "resume"
	case dispRefused:
		return "refused"
	default:
		return "new"
	}
}

// conflicts extracts the consent-needing entries for the overwrite prompt.
func conflicts(plans []entryPlan) []Conflict {
	var out []Conflict
	var dirs []string // conflicting folders; their contents follow them
	for _, p := range plans {
		if !p.needsConsent() || p.blockedAncestor || underAny(p.rel, dirs) {
			continue
		}
		if p.entry.Type == wire.EntryDir {
			dirs = append(dirs, p.rel)
		}
		c := Conflict{RelativePath: p.entry.RelativePath, IncomingSize: p.entry.Size, Kind: "differs", NonEmptyDir: p.nonEmptyDir}
		if st, err := os.Lstat(p.target); err == nil {
			c.LocalSize = st.Size()
			c.LocalModTime = st.ModTime()
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
		} else if p.disp == dispConflict {
			c.Kind = "file vs folder" // a file sits where a parent folder goes
		}
		out = append(out, c)
	}
	return out
}

func underAny(rel string, dirs []string) bool {
	for _, d := range dirs {
		if strings.HasPrefix(rel, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// applyPolicy turns the user's answer for conflicting entries into plan
// changes. Replace never deletes a non-empty folder; keep both moves each
// conflicting entry (and, for a folder, everything under it) to a free
// "name (n)" spelling.
func applyPolicy(plans []entryPlan, policy ConflictPolicy, c *classifier) {
	switch policy {
	case ConflictReplace:
		for i := range plans {
			p := &plans[i]
			if p.needsConsent() && p.nonEmptyDir {
				p.disp = dispRefused
				p.skipReason = "a folder with files is in the way (fsend never deletes a folder's contents)"
			}
		}
	case ConflictRename:
		taken := make(map[string]struct{}, len(plans))
		for i := range plans {
			taken[c.sem.nameKey(plans[i].rel)] = struct{}{}
		}
		type move struct{ from, to string }
		var moves []move
		for i := range plans {
			p := &plans[i]
			if p.blockedAncestor || p.disp == dispRefused {
				continue
			}
			moved := false
			for _, m := range moves {
				if rest, ok := strings.CutPrefix(p.rel, m.from+string(filepath.Separator)); ok {
					c.retarget(p, filepath.Join(m.to, rest))
					moved = true
					break
				}
			}
			if moved || !p.needsConsent() {
				continue
			}
			to := freeName(p.rel, c, taken)
			if p.entry.Type == wire.EntryDir {
				moves = append(moves, move{p.rel, to})
			}
			c.retarget(p, to)
		}
	}
}

// retarget points p at a fresh relative path, which is absent on disk.
func (c *classifier) retarget(p *entryPlan, rel string) {
	p.rel = rel
	p.target = filepath.Join(c.targetDir, rel)
	p.renamed = true
	p.nonEmptyDir = false
	p.disp = dispNew
	if p.entry.Type == wire.EntryFile {
		// An earlier interrupted keep-both run may have left a partial.
		c.resumeCandidate(p)
		if p.disp == dispResume {
			if blocks, err := hashPartialPrefix(p.partial, int64(p.resumeOffset), func(int64) {}); err == nil {
				p.prefixBlocks = blocks
			} else {
				c.ps.remove(p.rel)
				p.disp, p.resumeOffset, p.partial = dispNew, 0, ""
			}
		}
	}
}

// freeName returns "name (n).ext" beside rel, unused on disk and in this
// transfer under the filesystem's name semantics.
func freeName(rel string, c *classifier, taken map[string]struct{}) string {
	dir, base := filepath.Split(rel)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" { // ".bashrc" has no stem to number
		stem, ext = base, ""
	}
	for n := 1; ; n++ {
		cand := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))
		key := c.sem.nameKey(cand)
		if _, used := taken[key]; used {
			continue
		}
		if _, err := os.Lstat(filepath.Join(c.targetDir, cand)); err == nil {
			continue
		}
		taken[key] = struct{}{}
		return cand
	}
}
