package transfer

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/polius/fsend/internal/wire"
)

// eventLog records the order the sender and receiver callbacks fire in.
type eventLog struct {
	mu  sync.Mutex
	evs []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	l.evs = append(l.evs, e)
	l.mu.Unlock()
}

func (l *eventLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.evs, ",")
}

// The HELLO reply of a file receiver only lets the listing through: the
// sender must hear "accepted" after the receiver's prompt, never before it,
// and not at all on a decline. Stream receivers ask first, so for them the
// reply is the consent.
func TestSend_AcceptedFiresAfterTheReceiverDecides(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "f.txt"), []byte("data"))
	sources, err := Walk([]string{filepath.Join(src, "f.txt")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		mode     wire.TransferMode
		password string
		accept   bool
		want     string
	}{
		{"files accept", wire.ModeFiles, "", true, "peer,prompt,accepted"},
		{"files decline", wire.ModeFiles, "", false, "peer,prompt"},
		{"files password", wire.ModeFiles, "pw", true, "peer,password-ok,prompt,accepted"},
		{"stream accept", wire.ModeStream, "", true, "prompt,peer,accepted"},
		{"stream decline", wire.ModeStream, "", false, "prompt,peer"},
		{"stream password", wire.ModeStream, "pw", true, "prompt,peer,accepted,password-ok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var log eventLog
			so := SendOptions{
				Mode: c.mode, Password: c.password,
				OnPeer:       func(_ string, _ bool) { log.add("peer") },
				OnPasswordOK: func() { log.add("password-ok") },
				OnAccepted:   func() { log.add("accepted") },
			}
			if c.mode == wire.ModeFiles {
				so.Sources = sources
			} else {
				so.Stream, so.IsText, so.DisplayName = strings.NewReader("hello"), true, "t.txt"
			}
			ro := RecvOptions{
				TargetDir: t.TempDir(), Password: c.password,
				Accept: func(wire.SenderHello, ClassifySummary) bool {
					log.add("prompt")
					return c.accept
				},
			}
			_, _ = runTransfer(t, so, ro)
			if got := log.String(); got != c.want {
				t.Errorf("events = %s, want %s", got, c.want)
			}
		})
	}
}
