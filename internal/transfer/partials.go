package transfer

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zeebo/blake3"

	"github.com/polius/fsend/internal/wire"

	"github.com/polius/fsend/internal/conv"
)

// stateDirName is the hidden folder under the receive directory that holds
// in-flight partials. Keeping them in one place (not next to each target)
// means fixed-length names, one folder to clean, and nothing that looks like
// a half-written user file.
const stateDirName = ".fsend"

// partialMaxAge is how long an untouched partial survives before a later
// receive into the same folder sweeps it.
const partialMaxAge = 7 * 24 * time.Hour

// PartialDir is where partial downloads for targetDir are kept.
func PartialDir(targetDir string) string { return filepath.Join(targetDir, stateDirName) }

// partialStore manages <targetDir>/.fsend/<id>.partial plus a <id>.meta
// sidecar recording which source version the bytes belong to.
type partialStore struct {
	dir string
	// absent caches "no state folder exists": then no partial can exist
	// either, and lookups and removals are skipped (a re-send of 500,000
	// unchanged files otherwise Lstats and unlinks twice per file).
	absent bool
}

func newPartialStore(targetDir string) *partialStore {
	return &partialStore{dir: PartialDir(targetDir)}
}

// partialMeta identifies the source a partial was written from. A partial
// whose meta does not match the incoming entry belongs to another file (or
// an older version) and is discarded instead of resumed.
type partialMeta struct {
	Path    string `json:"path"`
	Size    uint64 `json:"size"`
	ModTime int64  `json:"mtime"`
}

// paths returns the data and sidecar paths for a target relative path.
func (ps *partialStore) paths(rel string) (data, meta string) {
	sum := blake3.Sum256([]byte(filepath.ToSlash(rel)))
	id := hex.EncodeToString(sum[:16])
	return filepath.Join(ps.dir, id+".partial"), filepath.Join(ps.dir, id+".meta")
}

// ensure creates the state dir (owner-only) and fails closed if something
// other than a real directory sits there.
func (ps *partialStore) ensure() error {
	st, err := os.Lstat(ps.dir)
	switch {
	case err == nil && st.IsDir():
		return nil
	case err == nil:
		return fmt.Errorf("%s exists and is not a folder", stateDirName)
	case !os.IsNotExist(err):
		return err
	}
	if err := os.Mkdir(ps.dir, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	ps.absent = false
	return nil
}

// lookup returns the partial for rel when its sidecar matches e. Mismatched,
// oversized or orphaned partials are removed.
func (ps *partialStore) lookup(rel string, e wire.ListingEntry) (string, int64, bool) {
	if ps.absent {
		return "", 0, false
	}
	data, meta := ps.paths(rel)
	st, err := os.Lstat(data)
	if err != nil {
		return "", 0, false
	}
	if !st.Mode().IsRegular() {
		return "", 0, false // never touch a planted link or folder
	}
	var m partialMeta
	b, err := os.ReadFile(meta)
	if err != nil || json.Unmarshal(b, &m) != nil ||
		m.Path != filepath.ToSlash(rel) || m.Size != e.Size || m.ModTime != e.ModTimeSec ||
		conv.Uint64(st.Size()) > e.Size {
		ps.remove(rel)
		return "", 0, false
	}
	return data, st.Size(), true
}

// create starts a fresh partial for rel, recording e as its source. Only a
// file that can ever resume (more than one hash block) gets a sidecar: the
// create-and-delete of one per small file was most of the per-file cost of
// a folder of many small files. The partial is created with the entry's
// final mode, so the commit needn't chmod it; fresh reports that (false
// when a stale partial was reused, whose mode the caller still sets). The
// state folder is owner-only, so the mode reveals nothing in flight.
func (ps *partialStore) create(rel string, e wire.ListingEntry) (f *os.File, path string, fresh bool, err error) {
	if err := ps.ensure(); err != nil {
		return nil, "", false, err
	}
	data, meta := ps.paths(rel)
	for _, p := range []string{data, meta} {
		if st, err := os.Lstat(p); err == nil && !st.Mode().IsRegular() {
			return nil, "", false, fmt.Errorf("partial %s is not a regular file", filepath.Base(p))
		}
	}
	if canResume(e.Size) {
		b, _ := json.Marshal(partialMeta{Path: filepath.ToSlash(rel), Size: e.Size, ModTime: e.ModTimeSec})
		if err := os.WriteFile(meta, b, 0o600); err != nil {
			return nil, "", false, err
		}
	}
	perm := os.FileMode(0o600)
	if e.Mode != 0 {
		perm = fileMode(e.Mode)
	}
	f, err = os.OpenFile(data, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
	if err == nil {
		return f, data, e.Mode != 0, nil
	}
	if !os.IsExist(err) {
		return nil, "", false, err
	}
	// A stale partial from an earlier run: truncate it in place.
	f, err = os.OpenFile(data, os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, "", false, err
	}
	return f, data, false, nil
}

// canResume reports whether a file of this size could ever resume: a
// resume needs at least one whole hash block before the last byte.
func canResume(size uint64) bool { return size > hashBlockSize }

// remove drops rel's partial and sidecar.
func (ps *partialStore) remove(rel string) {
	if ps.absent {
		return
	}
	data, meta := ps.paths(rel)
	_ = os.Remove(data)
	_ = os.Remove(meta)
}

// dropMeta removes the sidecar once the data has been committed.
func (ps *partialStore) dropMeta(rel string) {
	_, meta := ps.paths(rel)
	_ = os.Remove(meta)
}

// sweep deletes partials untouched for partialMaxAge, and sidecars whose
// data is gone.
func (ps *partialStore) sweep() {
	ents, err := os.ReadDir(ps.dir)
	if err != nil {
		ps.absent = os.IsNotExist(err)
		return
	}
	ps.absent = false
	now := time.Now()
	for _, e := range ents {
		name := e.Name()
		p := filepath.Join(ps.dir, name)
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		switch {
		case strings.HasSuffix(name, ".partial"):
			if now.Sub(info.ModTime()) > partialMaxAge {
				_ = os.Remove(p)
				_ = os.Remove(strings.TrimSuffix(p, ".partial") + ".meta")
			}
		case strings.HasSuffix(name, ".meta"):
			if _, err := os.Lstat(strings.TrimSuffix(p, ".meta") + ".partial"); os.IsNotExist(err) && now.Sub(info.ModTime()) > time.Hour {
				_ = os.Remove(p)
			}
		}
	}
}

// tidy removes the state dir when nothing is left in it.
func (ps *partialStore) tidy() { _ = os.Remove(ps.dir) }

// isReserved reports whether rel would land inside the state dir.
func isReserved(rel string) bool {
	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	return strings.EqualFold(first, stateDirName)
}
