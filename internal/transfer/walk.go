package transfer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// Source is one entry to send: its wire listing metadata plus the local
// path to read (empty for stream mode).
type Source struct {
	Entry   wire.ListingEntry
	AbsPath string // sender-local absolute path; "" for synthetic/stream
	// LinkTarget is where this entry actually lives when it came from a followed
	// symlink — a direct link's own target, or the resolved source path for a
	// file reached through a symlinked directory. Sender-side only (never sent
	// on the wire — the entry travels as a plain file); annotates the preview as
	// "name (→ target)".
	LinkTarget string
}

// Walk expands CLI arguments into an ordered Source list — files and
// directories (recursively). Symlinks are followed to their target content:
// fsend is a send tool, so the recipient receives the pointed-to file/dir, not
// a dangling link. A symlink whose target is missing, unreadable, or cyclic is
// a hard error (ErrUnsendableSymlink). No file contents are read here; the
// BLAKE3 root is computed inline at send time.
//
// RelativePath is rooted at each argument's base name, so `~/proj/foo` lands
// as `foo/...`. Duplicate roots and case-only collisions (the receiver may be
// on a case-insensitive filesystem) are rejected up front.
func Walk(paths []string, excludes []string) ([]Source, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("walk: no paths provided")
	}
	m, err := newExcludeMatcher(excludes)
	if err != nil {
		return nil, err
	}

	var out []Source
	seen := make(map[string]string) // lowercased relpath → original (collision check)
	roots := make(map[string]string, len(paths))
	for _, raw := range paths {
		abs, err := filepath.Abs(raw)
		if err != nil {
			return nil, fmt.Errorf("walk: %s: %w", raw, err)
		}
		st, err := os.Lstat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("%w: %s", fserrors.ErrSourceNotFound, raw)
			}
			return nil, fmt.Errorf("walk: %s: %w", raw, err)
		}
		// `.` / `./` / `foo/.` means "send this directory's contents", so its
		// children land at the top level instead of inside a wrapper folder.
		if IsContentsRef(raw) && st.IsDir() {
			children, err := os.ReadDir(abs)
			if err != nil {
				return nil, fmt.Errorf("walk: readdir %s: %w", abs, err)
			}
			sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
			for _, c := range children {
				cAbs := filepath.Join(abs, c.Name())
				cst, err := os.Lstat(cAbs)
				if err != nil {
					return nil, fmt.Errorf("walk: lstat %s: %w", cAbs, err)
				}
				if err := addEntry(&out, seen, nil, "", cAbs, c.Name(), cst, m); err != nil {
					return nil, err
				}
			}
			continue
		}
		// A named argument must be sendable content. A fifo/socket/device
		// found inside a folder is skipped silently (addEntry), but one the
		// user pointed at directly deserves an honest refusal — the silent
		// skip would misreport it as an empty folder. A symlink argument is
		// resolved by addEntry, which already errors if its target is
		// unsendable.
		if mode := st.Mode(); !mode.IsDir() && !mode.IsRegular() && mode&os.ModeSymlink == 0 {
			return nil, fmt.Errorf("%w: cannot send %s — not a regular file (%s)", fserrors.ErrUsage, raw, fileKind(mode))
		}
		// Preflight an unreadable file now, before a code is issued —
		// otherwise the E010 fires only after a receiver accepts, wasting
		// the round-trip. Top-level arguments only: opening every file of a
		// large tree here would double the syscall bill for a rare failure,
		// so files inside directories keep failing at send time.
		if st.Mode().IsRegular() {
			f, err := os.Open(abs)
			if err != nil {
				return nil, fmt.Errorf("walk: %s: %w", raw, err)
			}
			_ = f.Close()
		}
		root := filepath.Base(abs)
		if prev, ok := roots[strings.ToLower(root)]; ok {
			return nil, fmt.Errorf("%w: %s and %s would both arrive as %q — rename one before sending", fserrors.ErrUsage, prev, raw, root)
		}
		roots[strings.ToLower(root)] = raw
		if err := addEntry(&out, seen, nil, "", abs, root, st, m); err != nil {
			return nil, err
		}
	}
	// Assign indices in final order.
	for i := range out {
		out[i].Entry.Index = uint32(i)
	}
	return out, nil
}

// addEntry appends one filesystem entry and recurses into directories. rel is
// the forward-slash path as seen inside the transfer. stack holds the FileInfo
// of every directory currently on the recursion path, so a followed symlink
// that points back into an ancestor is caught as a cycle instead of recursing
// forever. srcPrefix is the real-source path of the parent directory when we're
// inside a followed symlink (empty in the user's real tree); it lets every
// symlink-derived file carry its true origin for the preview annotation.
func addEntry(out *[]Source, seen map[string]string, stack []os.FileInfo, srcPrefix, abs, rel string, st os.FileInfo, m excludeMatcher) error {
	if m.match(rel) {
		return nil
	}
	if prev, ok := seen[strings.ToLower(rel)]; ok {
		return fmt.Errorf("%w: %s and %s collide on a case-insensitive filesystem — rename one", fserrors.ErrUsage, prev, rel)
	}
	seen[strings.ToLower(rel)] = rel

	// Follow symlinks to their target. os.Stat resolves the whole chain (and
	// reports ELOOP for a self-referential one); a missing or unreadable
	// target is a hard stop. linkTarget is the raw target of a direct symlink.
	linkTarget := ""
	if st.Mode()&os.ModeSymlink != 0 {
		target, _ := os.Readlink(abs)
		linkTarget = target
		resolved, err := os.Stat(abs)
		if err != nil {
			return symlinkError(rel, target, err)
		}
		st = resolved
	}

	// src is where this entry actually lives, for the preview annotation: a
	// direct symlink's own target, or — for a file reached through a followed
	// symlinked directory — the parent's source path plus this base name. "" for
	// a genuine entry in the user's tree (no annotation). It also becomes the
	// srcPrefix passed to children, composing through nested symlinks.
	src := linkTarget
	if src == "" && srcPrefix != "" {
		base := rel
		if i := strings.LastIndex(rel, "/"); i >= 0 {
			base = rel[i+1:]
		}
		src = srcPrefix + "/" + base
	}

	mode := st.Mode()
	e := wire.ListingEntry{
		RelativePath: filepath.ToSlash(rel),
		Mode:         uint32(mode.Perm()),
		ModTimeSec:   st.ModTime().Unix(),
	}

	switch {
	case mode.IsDir():
		// Cycle guard: only meaningful when we got here through a symlink (a
		// real directory tree can't contain itself). Compare this directory's
		// identity against every ancestor on the stack.
		if linkTarget != "" {
			for _, anc := range stack {
				if os.SameFile(st, anc) {
					return fmt.Errorf("%w: %s → %s loops back into the transfer", fserrors.ErrUnsendableSymlink, rel, linkTarget)
				}
			}
		}
		e.Type = wire.EntryDir
		*out = append(*out, Source{Entry: e, AbsPath: abs})
		children, err := os.ReadDir(abs)
		if err != nil {
			return fmt.Errorf("walk: readdir %s: %w", abs, err)
		}
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		stack = append(stack, st)
		for _, c := range children {
			cAbs := filepath.Join(abs, c.Name())
			cRel := rel + "/" + c.Name()
			cst, err := os.Lstat(cAbs)
			if err != nil {
				return fmt.Errorf("walk: lstat %s: %w", cAbs, err)
			}
			if err := addEntry(out, seen, stack, src, cAbs, cRel, cst, m); err != nil {
				return err
			}
		}
		return nil

	case mode.IsRegular():
		e.Type = wire.EntryFile
		e.Size = uint64(st.Size())
		*out = append(*out, Source{Entry: e, AbsPath: abs, LinkTarget: src})
		return nil

	default:
		// fifo/device/socket can't be copied. A plain one is skipped silently
		// (as before); one a symlink explicitly resolves to is surfaced rather
		// than silently dropping a target the user pointed at.
		if linkTarget != "" {
			return fmt.Errorf("%w: %s → %s is not a regular file", fserrors.ErrUnsendableSymlink, rel, linkTarget)
		}
		return nil
	}
}

// fileKind names a non-regular file mode for the "cannot send" usage error.
func fileKind(m os.FileMode) string {
	switch {
	case m&os.ModeNamedPipe != 0:
		return "fifo"
	case m&os.ModeSocket != 0:
		return "socket"
	case m&os.ModeDevice != 0:
		return "device"
	default:
		return "special file"
	}
}

// symlinkError classifies a failure to resolve a symlink's target into a
// user-facing ErrUnsendableSymlink with the offending path and cause.
func symlinkError(rel, target string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: broken link %s → %s (target does not exist)", fserrors.ErrUnsendableSymlink, rel, target)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%w: cannot read target of %s → %s (permission denied)", fserrors.ErrUnsendableSymlink, rel, target)
	default:
		// Includes ELOOP from a self-referential chain (a → b → a); the OS
		// message ("too many levels of symbolic links") makes the cause clear.
		return fmt.Errorf("%w: cannot resolve %s → %s: %v", fserrors.ErrUnsendableSymlink, rel, target, err)
	}
}

// IsContentsRef reports whether p explicitly names a directory's *contents*
// rather than the directory itself: ".", "./", or any path ending in "/.".
// A bare trailing slash ("foo/") is intentionally NOT treated this way —
// shells/tab-completion append it freely, and silently spreading the contents
// would be a nasty surprise (rsync's well-known trailing-slash footgun).
func IsContentsRef(p string) bool {
	s := strings.TrimRight(filepath.ToSlash(p), "/")
	return s == "." || strings.HasSuffix(s, "/.")
}

// CountFiles returns the number of regular files + symlinks in sources — the
// user-facing "N files" count (directories aren't counted).
func CountFiles(sources []Source) int {
	n := 0
	for _, s := range sources {
		if s.Entry.Type != wire.EntryDir {
			n++
		}
	}
	return n
}

// TotalBytes sums the sizes across sources.
func TotalBytes(sources []Source) uint64 {
	var t uint64
	for _, s := range sources {
		t += s.Entry.Size
	}
	return t
}
