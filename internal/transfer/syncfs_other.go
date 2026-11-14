//go:build !linux

package transfer

import "os"

// syncWholeFS has no portable equivalent here: the caller fsyncs each file.
func syncWholeFS(*os.File) (bool, error) { return false, nil }
