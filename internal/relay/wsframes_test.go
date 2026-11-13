package relay

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func frame(op byte, payload []byte) []byte {
	b := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		b = append(b, byte(n))
	case n < 1<<16:
		b = append(b, 126, byte(n>>8), byte(n))
	default:
		b = append(b, 127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	return append(b, payload...)
}

func TestWSFrameTracker(t *testing.T) {
	big := make([]byte, 70000)
	stream := bytes.Join([][]byte{frame(2, []byte("hi")), frame(2, big), frame(2, make([]byte, 300)), frame(8, []byte{3, 232})}, nil)
	for _, chunk := range []int{1, 3, 7, 4096, len(stream)} {
		var out bytes.Buffer
		tr := &wsFrameTracker{w: &out}
		for i := 0; i < len(stream); i += chunk {
			end := i + chunk
			if end > len(stream) {
				end = len(stream)
			}
			tr.Write(stream[i:end])
		}
		if !bytes.Equal(out.Bytes(), stream) || !tr.CloseFrameSeen() {
			t.Fatalf("chunk %d: close seen %v, bytes equal %v", chunk, tr.CloseFrameSeen(), bytes.Equal(out.Bytes(), stream))
		}
	}
	// A data frame last: no close.
	tr := &wsFrameTracker{w: &bytes.Buffer{}}
	tr.Write(append(frame(8, nil)[:0:0], frame(2, []byte("x"))...))
	if tr.CloseFrameSeen() {
		t.Fatal("close reported without a close frame")
	}
	// Raw bytes (HTTPUpgrade) are recognised as not WebSocket framing.
	raw := make([]byte, 4096)
	rand.Read(raw)
	tr = &wsFrameTracker{w: &bytes.Buffer{}}
	tr.Write(raw)
	tr.Write(frame(8, nil))
	if tr.CloseFrameSeen() {
		t.Fatal("random data must disable close detection")
	}
}
