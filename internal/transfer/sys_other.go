//go:build !linux && !darwin && !freebsd && !windows

package transfer

import "os"

func readUmask() uint32 { return 0o022 }

func freeSpace(string) (uint64, bool) { return 0, false }

func fsyncData(f *os.File) error { return f.Sync() }

func syncBarrier(*os.File) error { return nil }

func isCrossDevice(error) bool { return false }
