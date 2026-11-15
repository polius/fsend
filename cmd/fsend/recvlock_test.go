package main

import (
	"errors"
	"os"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
)

// One fsend per code on a device: the second is told so at once, as E003,
// and the lock goes with its holder.
func TestLockReceive(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const c = "abc-defg-jkm"

	release, err := lockReceive(c)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if _, err := lockReceive(c); !errors.Is(err, errReceivingHere) || !errors.Is(err, fserrors.ErrCodeAlreadyClaimed) {
		t.Fatalf("second lock on the same code: got %v, want errReceivingHere (E003)", err)
	}
	other, err := lockReceive("xyz-pqrs-tuv")
	if err != nil {
		t.Fatalf("another code is locked out: %v", err)
	}
	other()

	rp, err := resumePath(c)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := os.Stat(rp + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the lock file outlived its holder: %v", err)
	}
	again, err := lockReceive(c)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again()
}

// A lock file left behind (a holder that was killed) does not lock anyone
// out: the system dropped the lock with the process.
func TestLockReceive_StaleFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const c = "abc-defg-jkm"
	rp, err := resumePath(c)
	if err != nil {
		t.Fatal(err)
	}
	release, err := lockReceive(c)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := os.WriteFile(rp+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	release, err = lockReceive(c)
	if err != nil {
		t.Fatalf("a stale lock file locked the code: %v", err)
	}
	release()
}

// With no usable config directory the receive goes ahead unlocked.
func TestLockReceive_NoConfigDir(t *testing.T) {
	dir := t.TempDir()
	blocker := dir + "/file"
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", blocker) // a file where the directory should be
	release, err := lockReceive("abc-defg-jkm")
	if err != nil {
		t.Fatalf("got %v, want an unlocked go-ahead", err)
	}
	release()
}
