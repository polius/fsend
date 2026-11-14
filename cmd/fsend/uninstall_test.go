package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Uninstall drops the install dir from PATH only when fsend is all it
// holds; a dir shared with other tools keeps its entry.
func TestOnlyFsendIn(t *testing.T) {
	cases := map[string]struct {
		files []string
		want  bool
	}{
		"just fsend":            {[]string{"fsend.exe"}, true},
		"fsend and its .old":    {[]string{"fsend.exe", "fsend.exe.old"}, true},
		"case differs":          {[]string{"FSEND.EXE"}, true},
		"another tool":          {[]string{"fsend.exe", "rg.exe"}, false},
		"a subdirectory":        {[]string{"fsend.exe", "lib/"}, false},
		"fsend already removed": {nil, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range c.files {
				p := filepath.Join(dir, f)
				var err error
				if f[len(f)-1] == '/' {
					err = os.Mkdir(p, 0o755)
				} else {
					err = os.WriteFile(p, nil, 0o644)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := onlyFsendIn(dir, filepath.Join(dir, "fsend.exe")); got != c.want {
				t.Errorf("onlyFsendIn = %v, want %v", got, c.want)
			}
		})
	}
	if onlyFsendIn(filepath.Join(t.TempDir(), "missing"), "fsend.exe") {
		t.Error("an unreadable dir counted as fsend-only")
	}
}
