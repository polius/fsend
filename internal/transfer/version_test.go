package transfer

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/polius/fsend/internal/wire"
)

// tapConn records every byte written to a control stream.
type tapConn struct {
	io.ReadWriteCloser
	mu  sync.Mutex
	out bytes.Buffer
}

func (t *tapConn) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.out.Write(p)
	t.mu.Unlock()
	return t.ReadWriteCloser.Write(p)
}

// frameVersions parses a recorded control stream into (type, version) pairs.
func frameVersions(t *testing.T, b []byte) [][2]byte {
	t.Helper()
	var out [][2]byte
	for len(b) >= 6 {
		n := binary.BigEndian.Uint32(b[2:6])
		out = append(out, [2]byte{b[1], b[0]})
		if int(n) > len(b)-6 {
			t.Fatalf("truncated frame in the recording")
		}
		b = b[6+n:]
	}
	return out
}

// A sender that advertises a newer version (4) must still settle on 3 with
// a receiver that speaks only 3: the HELLOs carry MinProtocolVersion in
// their header, the sender records 3 as the session's version, and every
// later frame on both sides is stamped 3.
//
// While MinProtocolVersion == ProtocolVersion this can't tell a stamp of
// the negotiated version from a stamp of the build's constant;
// TestStampFollowsNegotiation and wire's TestReadControl_RequiresSessionVersion
// pin that rule. When ProtocolVersion is raised, cap this receiver at 3 so
// it keeps standing in for a released v3 peer.
func TestSend_NewerBuildTalksToV3Peer(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), []byte("hello across versions"))
	sources, err := Walk([]string{filepath.Join(src, "a.txt")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctrlA, ctrlB := net.Pipe()
	dataA, dataB := net.Pipe()
	sTap, rTap := &tapConn{ReadWriteCloser: ctrlA}, &tapConn{ReadWriteCloser: ctrlB}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var se, re error
	var negotiated uint8
	wg.Add(2)
	go func() {
		defer wg.Done()
		se = send(ctx, &Streams{Control: sTap, Data: dataA},
			SendOptions{Mode: wire.ModeFiles, Sources: sources, advertiseMax: wire.ProtocolVersion + 1}, &negotiated)
		_ = ctrlA.Close()
		_ = dataA.Close()
	}()
	go func() {
		defer wg.Done()
		re = Recv(ctx, &Streams{Control: rTap, Data: dataB}, RecvOptions{TargetDir: dst})
		_ = ctrlB.Close()
		_ = dataB.Close()
	}()
	wg.Wait()
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if got := string(mustRead(t, filepath.Join(dst, "a.txt"))); got != "hello across versions" {
		t.Fatalf("content = %q", got)
	}
	if negotiated != wire.ProtocolVersion {
		t.Errorf("the sender recorded version %d for the session, want %d", negotiated, wire.ProtocolVersion)
	}
	for side, rec := range map[string][]byte{"sender": sTap.out.Bytes(), "receiver": rTap.out.Bytes()} {
		frames := frameVersions(t, rec)
		if len(frames) < 2 {
			t.Fatalf("%s wrote %d frames", side, len(frames))
		}
		for _, f := range frames {
			ft, ver := wire.FrameType(f[0]), f[1]
			want := byte(wire.ProtocolVersion) // negotiated: min(4, 3)
			if ft == wire.TypeHello || ft == wire.TypeHelloAck {
				want = wire.MinProtocolVersion
			}
			if ver != want {
				t.Errorf("%s frame %v stamped %d, want %d", side, ft, ver, want)
			}
		}
	}
}
