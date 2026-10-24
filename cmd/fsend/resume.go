package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/polius/fsend/internal/atomicfile"
	"github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/config"
)

// pairKey names one pairing. rendezvous drives discovery (mDNS name, LAN
// port, server slot); secret is the PAKE secret. A fresh transfer uses the
// code for both. After the first successful PAKE both peers switch to a
// key derived from the session binding, so a code read over a shoulder
// can't take over a dropped transfer: re-pairing needs the binding, which
// only the two original peers hold.
type pairKey struct {
	rendezvous string
	secret     string
}

func codeKey(c string) pairKey { return pairKey{rendezvous: c, secret: c} }

// resumeKey derives the post-PAKE key from a session binding. The
// rendezvous is code-shaped, so discovery treats it like any code; the
// secret carries the binding's full 256 bits.
func resumeKey(binding []byte) pairKey {
	mac := func(label string) []byte {
		h := hmac.New(sha256.New, binding)
		h.Write([]byte(label))
		return h.Sum(nil)
	}
	r := mac("fsend-resume-rendezvous-v1")
	letters := make([]byte, code.Length)
	for i := range letters {
		letters[i] = code.Alphabet[int(r[i])%len(code.Alphabet)]
	}
	l := string(letters)
	return pairKey{
		rendezvous: l[:3] + "-" + l[3:7] + "-" + l[7:],
		secret:     hex.EncodeToString(mac("fsend-resume-pake-v1")),
	}
}

// resumeTTL bounds how long a receiver keeps a resume key on disk. A
// sender waits at most an hour per round; a day covers any realistic
// rerun and keeps stale keys from piling up.
const resumeTTL = 24 * time.Hour

type resumeRecord struct {
	Rendezvous string    `json:"rendezvous"`
	Secret     string    `json:"secret"`
	Saved      time.Time `json:"saved"`
}

// resumePath names the receiver's resume key for c. The file name is a
// hash of the code; once a key exists the code alone no longer pairs, so
// the name reveals nothing useful.
func resumePath(c string) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte("fsend-resume-file-v1/" + c))
	return filepath.Join(dir, "resume", hex.EncodeToString(sum[:16])), nil
}

// saveResumeKey records k so a rerun `fsend <code>` on this device can
// resume after the process dies. Best effort: without it only in-process
// retries can resume.
func saveResumeKey(c string, k pairKey) {
	p, err := resumePath(c)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	b, err := json.Marshal(resumeRecord{Rendezvous: k.rendezvous, Secret: k.secret, Saved: time.Now()})
	if err != nil {
		return
	}
	_ = atomicfile.Write(p, b, 0o600)
}

// loadResumeKey returns the saved key for c, if a fresh one exists.
func loadResumeKey(c string) (pairKey, bool) {
	p, err := resumePath(c)
	if err != nil {
		return pairKey{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return pairKey{}, false
	}
	var r resumeRecord
	if json.Unmarshal(b, &r) != nil || r.Rendezvous == "" || r.Secret == "" || time.Since(r.Saved) > resumeTTL {
		_ = os.Remove(p)
		return pairKey{}, false
	}
	return pairKey{rendezvous: r.Rendezvous, secret: r.Secret}, true
}

// forgetResumeKey drops c's saved key once the transfer is done.
func forgetResumeKey(c string) {
	if p, err := resumePath(c); err == nil {
		_ = os.Remove(p)
	}
}
