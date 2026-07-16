// The attach socket's framed protocol, shared by the Unix (ptyhost.go)
// and Windows (ptyhost_windows.go) hosts and by `cux attach` / bridges:
//
//	[1 byte type][4 bytes big-endian length][payload]
//
//	0 out    — PTY output (host → client)
//	1 input  — keystrokes for the PTY (client → host)
//	2 resize — payload rows,cols as two big-endian uint16 (client → host)
//	3 ping   — keepalive, either direction, empty payload
//	4 size   — payload rows,cols as two big-endian uint16 (host → client):
//	           the size the shared PTY actually settled on (the tmux-rule
//	           intersection of every attached client), so a client whose
//	           own request lost the negotiation can still render at the
//	           real size instead of drifting out of sync with the stream.
package ptyhost

import (
	"encoding/binary"
	"errors"
	"io"
)

const (
	FrameOut byte = iota
	FrameInput
	FrameResize
	FramePing
	FrameSize
)

var errFrameTooBig = errors.New("ptyhost: frame exceeds limit")

const maxFrame = 1 << 20

func writeFrame(w io.Writer, typ byte, payload []byte) error {
	hdr := [5]byte{typ}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, errFrameTooBig
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// WriteFrame / ReadFrame are exported for `cux attach` and bridges.
func WriteFrame(w io.Writer, typ byte, payload []byte) error { return writeFrame(w, typ, payload) }
func ReadFrame(r io.Reader) (byte, []byte, error)            { return readFrame(r) }
