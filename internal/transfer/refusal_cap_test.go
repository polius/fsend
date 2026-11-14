package transfer

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polius/fsend/internal/wire"
)

// tooLong is a name every filesystem refuses, and only when the file is
// written: it passes the up-front path checks.
func tooLong(prefix string) string { return prefix + strings.Repeat("x", 300) + ".txt" }

// refusalRun is one transfer of names into dst with both sides' refusal
// callbacks recorded and the receiver's control frames captured.
type refusalRun struct {
	refusals
	more        int
	sendErr     error
	recvErr     error
	recvControl []byte
}

func runWriteRefusal(t *testing.T, names []string, dst string, senderWithout, receiverWithout wire.Capability) *refusalRun {
	t.Helper()
	run := &refusalRun{refusals: refusals{recv: map[string]string{}, send: map[uint32]string{}}}
	ctrlA, ctrlB := net.Pipe()
	dataA, dataB := net.Pipe()
	rTap := &tapConn{ReadWriteCloser: ctrlB}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s := &Streams{Control: ctrlA, Data: dataA}
		run.sendErr = Send(ctx, s, SendOptions{Mode: wire.ModeFiles, Sources: sourcesFor(t, names), withoutCaps: senderWithout,
			OnRefused:     func(i uint32, why string) { run.mu.Lock(); run.send[i] = why; run.mu.Unlock() },
			OnRefusedMore: func(n int) { run.mu.Lock(); run.more = n; run.mu.Unlock() }})
		s.Close()
	}()
	go func() {
		defer wg.Done()
		s := &Streams{Control: rTap, Data: dataB}
		run.recvErr = Recv(ctx, s, RecvOptions{TargetDir: dst, withoutCaps: receiverWithout,
			OnEntrySkipped: func(rel, why string) { run.mu.Lock(); run.recv[rel] = why; run.mu.Unlock() }})
		s.Close()
	}()
	wg.Wait()
	run.recvControl = rTap.out.Bytes()
	return run
}

// ackBody returns the payload of the TRANSFER_ACK in a recorded receiver
// control stream.
func ackBody(t *testing.T, rec []byte) []byte {
	t.Helper()
	for len(rec) >= 6 {
		n := int(binary.BigEndian.Uint32(rec[2:6]))
		if n > len(rec)-6 {
			t.Fatal("truncated frame in the recording")
		}
		if wire.FrameType(rec[1]) == wire.TypeTransferAck {
			return rec[6 : 6+n]
		}
		rec = rec[6+n:]
	}
	t.Fatal("the receiver never wrote a TRANSFER_ACK")
	return nil
}

// A name the receiver's filesystem refuses only when the file is written
// is reported to the sender in the final ack: both sides list it with the
// same reason, the neighbours arrive, and neither side fails the transfer
// (the CLI turns the report into E050 on both).
func TestWriteRefusal_ReachesTheSender(t *testing.T) {
	dst := t.TempDir()
	long := tooLong("report-")
	run := runWriteRefusal(t, []string{"good.txt", long, "after.txt"}, dst, 0, 0)
	if run.sendErr != nil || run.recvErr != nil {
		t.Fatalf("send=%v recv=%v, want both to finish", run.sendErr, run.recvErr)
	}
	for _, n := range []string{"good.txt", "after.txt"} {
		if _, err := os.Stat(filepath.Join(dst, n)); err != nil {
			t.Errorf("%s did not arrive: %v", n, err)
		}
	}
	if run.recv[long] != nameRefusedReason {
		t.Errorf("receiver's reason = %q, want %q", run.recv[long], nameRefusedReason)
	}
	if len(run.send) != 1 || run.send[1] != nameRefusedReason {
		t.Errorf("sender heard %v, want index 1 refused with %q", run.send, nameRefusedReason)
	}
	if run.more != 0 {
		t.Errorf("sender was told of %d unnamed refusals, want none", run.more)
	}
	if len(ackBody(t, run.recvControl)) == 0 {
		t.Error("the ack carried no report")
	}
}

// With nothing to report the ack is the empty frame it has always been,
// whatever the peers' capabilities.
func TestWriteRefusal_CleanTransferAcksEmpty(t *testing.T) {
	run := runWriteRefusal(t, []string{"a.txt", "b.txt"}, t.TempDir(), 0, 0)
	if run.sendErr != nil || run.recvErr != nil {
		t.Fatalf("send=%v recv=%v", run.sendErr, run.recvErr)
	}
	if b := ackBody(t, run.recvControl); len(b) != 0 {
		t.Errorf("a clean transfer's ack carries %d bytes, want none", len(b))
	}
}

// A sender without CapWriteRefusals (the released 1.16.1 advertises no
// capabilities at all) gets the empty ack it expects even when a name was
// refused at write time: it finishes as it does today, and the receiver
// still lists the file.
func TestWriteRefusal_OldSenderGetsAnEmptyAck(t *testing.T) {
	dst := t.TempDir()
	long := tooLong("old-sender-")
	run := runWriteRefusal(t, []string{"good.txt", long}, dst, wire.CapsSupported, 0)
	if run.sendErr != nil || run.recvErr != nil {
		t.Fatalf("send=%v recv=%v, want both to finish", run.sendErr, run.recvErr)
	}
	if b := ackBody(t, run.recvControl); len(b) != 0 {
		t.Errorf("the ack to a sender without the capability carries %d bytes, want none", len(b))
	}
	if len(run.send) != 0 {
		t.Errorf("sender heard %v through a capability it did not advertise", run.send)
	}
	if run.recv[long] == "" {
		t.Errorf("the receiver did not list the refused name: %v", run.recv)
	}
	if _, err := os.Stat(filepath.Join(dst, "good.txt")); err != nil {
		t.Errorf("good.txt did not arrive: %v", err)
	}
}

// A sender with the capability finishes against a receiver without it
// (1.16.1 always acks empty), reporting nothing it wasn't told.
func TestWriteRefusal_OldReceiverAcksEmpty(t *testing.T) {
	dst := t.TempDir()
	long := tooLong("old-receiver-")
	run := runWriteRefusal(t, []string{"good.txt", long}, dst, 0, wire.CapsSupported)
	if run.sendErr != nil || run.recvErr != nil {
		t.Fatalf("send=%v recv=%v, want both to finish", run.sendErr, run.recvErr)
	}
	if b := ackBody(t, run.recvControl); len(b) != 0 {
		t.Errorf("a receiver without the capability wrote a %d-byte ack", len(b))
	}
	if len(run.send) != 0 || run.more != 0 {
		t.Errorf("sender reported %v (+%d) without being told", run.send, run.more)
	}
	if _, err := os.Stat(filepath.Join(dst, "good.txt")); err != nil {
		t.Errorf("good.txt did not arrive: %v", err)
	}
}

// The report is bounded: past wire.MaxWriteRefusals the receiver names the
// first ones and counts the rest, and the sender passes both on.
func TestWriteRefusal_ReportIsBounded(t *testing.T) {
	const extra = 44
	names := []string{"good.txt"}
	for i := range wire.MaxWriteRefusals + extra {
		names = append(names, tooLong(fmt.Sprintf("%04d-", i)))
	}
	dst := t.TempDir()
	run := runWriteRefusal(t, names, dst, 0, 0)
	if run.sendErr != nil || run.recvErr != nil {
		t.Fatalf("send=%v recv=%v, want both to finish", run.sendErr, run.recvErr)
	}
	if len(run.recv) != wire.MaxWriteRefusals+extra {
		t.Errorf("receiver listed %d names, want %d", len(run.recv), wire.MaxWriteRefusals+extra)
	}
	if len(run.send) != wire.MaxWriteRefusals || run.more != extra {
		t.Errorf("sender heard %d names and %d more, want %d and %d", len(run.send), run.more, wire.MaxWriteRefusals, extra)
	}
	if n := len(ackBody(t, run.recvControl)); n > wire.MaxControlFrameSize/2 {
		t.Errorf("the report is %d bytes", n)
	}
	if _, err := os.Stat(filepath.Join(dst, "good.txt")); err != nil {
		t.Errorf("good.txt did not arrive: %v", err)
	}
}

// The report is the receiver's word: the sender passes on only refusals of
// files it actually sent, each once, and never more than it sent.
func TestReportWriteRefusals_IgnoresWhatWasNotSent(t *testing.T) {
	file := func(i uint32) Source {
		return Source{Entry: wire.ListingEntry{Index: i, RelativePath: fmt.Sprintf("f%d", i), Type: wire.EntryFile, Size: 1}}
	}
	sources := []Source{
		{Entry: wire.ListingEntry{Index: 0, RelativePath: "dir", Type: wire.EntryDir}},
		file(1), file(2), file(3), file(4),
	}
	decisions := map[uint32]wire.Decision{
		0: {Index: 0, Action: wire.DecisionSend},
		1: {Index: 1, Action: wire.DecisionSend},
		2: {Index: 2, Action: wire.DecisionSkip}, // the receiver kept its own
		3: {Index: 3, Action: wire.DecisionResume},
		4: {Index: 4, Action: wire.DecisionSend},
	}
	var got []uint32
	more := -1
	opts := SendOptions{Sources: sources,
		OnRefused:     func(i uint32, _ string) { got = append(got, i) },
		OnRefusedMore: func(n int) { more = n }}

	reportWriteRefusals(nil, decisions, opts)
	if got != nil || more != -1 {
		t.Fatalf("an empty ack reported %v, %d", got, more)
	}
	reportWriteRefusals(&wire.TransferResult{Refused: 1 << 30, Entries: []wire.WriteRefusal{
		{Index: 0, Reason: "a folder"}, {Index: 1, Reason: "sent"}, {Index: 1, Reason: "twice"},
		{Index: 2, Reason: "was skipped"}, {Index: 99, Reason: "never listed"}, {Index: 3, Reason: "resumed"},
	}}, decisions, opts)
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("reported %v, want [1 3]", got)
	}
	// Three files were sent and two are named: at most one more can exist,
	// whatever count the receiver claims.
	if more != 1 {
		t.Errorf("more = %d, want 1", more)
	}

	// Entries past the bound are not read at all.
	got, more = nil, -1
	over := &wire.TransferResult{Entries: make([]wire.WriteRefusal, wire.MaxWriteRefusals+1)}
	for i := range over.Entries {
		over.Entries[i].Index = 99
	}
	over.Entries[wire.MaxWriteRefusals].Index = 4
	reportWriteRefusals(over, decisions, opts)
	if got != nil || more != -1 {
		t.Errorf("an over-long report yielded %v, %d", got, more)
	}
}

// clipReason keeps a reason within the wire bound without cutting a rune.
func TestClipReason(t *testing.T) {
	if got := clipReason("short"); got != "short" {
		t.Errorf("clipReason(short) = %q", got)
	}
	long := strings.Repeat("é", wire.MaxRefusalReason) // 2 bytes each
	got := clipReason(long)
	if len(got) > wire.MaxRefusalReason || len(got) < wire.MaxRefusalReason-1 || !strings.HasPrefix(long, got) || strings.ContainsRune(got, '�') {
		t.Errorf("clipReason cut to %d bytes: %q", len(got), got)
	}
}
