package relay

import "io"

// wsFrameTracker passes server-to-client WebSocket bytes through unchanged
// and follows only the frame headers, to learn whether the backend's last
// frame was a close frame. Payloads are never inspected. If the bytes do not
// look like valid server frames (for example an HTTPUpgrade raw stream), the
// tracker gives up for the rest of the connection.
type wsFrameTracker struct {
	w         io.Writer
	invalid   bool
	hdr       [10]byte
	hn        int    // header bytes collected
	need      int    // header length for the current frame
	remaining uint64 // payload bytes left in the current frame
	op        byte
	closeSeen bool
}

func (t *wsFrameTracker) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if !t.invalid {
		t.scan(p[:n])
	}
	return n, err
}

// CloseFrameSeen reports whether a well-formed close frame was the last
// complete frame sent.
func (t *wsFrameTracker) CloseFrameSeen() bool {
	return !t.invalid && t.closeSeen && t.remaining == 0 && t.hn == 0
}

func (t *wsFrameTracker) scan(p []byte) {
	for len(p) > 0 && !t.invalid {
		if t.remaining > 0 {
			k := uint64(len(p))
			if k > t.remaining {
				k = t.remaining
			}
			p = p[k:]
			t.remaining -= k
			continue
		}
		t.hdr[t.hn] = p[0]
		t.hn++
		p = p[1:]
		if t.hn == 2 {
			b0, b1 := t.hdr[0], t.hdr[1]
			t.op = b0 & 0x0f
			l := b1 & 0x7f
			switch {
			case b0&0x70 != 0, b1&0x80 != 0: // reserved bits, or a masked server frame
				t.invalid = true
				return
			case t.op > 2 && t.op < 8, t.op > 10:
				t.invalid = true
				return
			case t.op >= 8 && (l > 125 || b0&0x80 == 0): // control frames: short, unfragmented
				t.invalid = true
				return
			}
			switch l {
			case 126:
				t.need = 4
			case 127:
				t.need = 10
			default:
				t.need = 2
			}
		}
		if t.hn >= 2 && t.hn == t.need {
			var n uint64
			switch t.need {
			case 2:
				n = uint64(t.hdr[1] & 0x7f)
			case 4:
				n = uint64(t.hdr[2])<<8 | uint64(t.hdr[3])
			case 10:
				for _, b := range t.hdr[2:10] {
					n = n<<8 | uint64(b)
				}
			}
			t.closeSeen = t.op == 8
			t.remaining = n
			t.hn = 0
		}
	}
}
