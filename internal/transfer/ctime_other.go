//go:build !linux && !openbsd && !darwin && !freebsd

package transfer

import "os"

// changeTime: os.FileInfo carries no change time here (Windows keeps one,
// but not in what os.Stat returns); a file's state is then its identity,
// size and modification time.
func changeTime(os.FileInfo) int64 { return 0 }
