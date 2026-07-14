//go:build windows

// Attachable sessions require a Unix PTY; on Windows the wrapper falls
// back to plain stdio inheritance (ConPTY support can follow).
package ptyhost

import (
	"errors"
	"io"
	"os"
	"syscall"
)

var ErrUnsupported = errors.New("ptyhost: not supported on windows")

const (
	FrameOut byte = iota
	FrameInput
	FrameResize
	FramePing
)

type Host struct{}

func New(sockPath string, inputOK bool) (*Host, error)       { return nil, ErrUnsupported }
func (h *Host) TTY() *os.File                                { return nil }
func (h *Host) TTYDup() (*os.File, error)                    { return nil, ErrUnsupported }
func (h *Host) Pump()                                        {}
func (h *Host) BroadcastWriter() io.Writer                   { return io.Discard }
func (h *Host) Close()                                       {}
func SysProcAttr() *syscall.SysProcAttr                      { return nil }
func WriteFrame(w io.Writer, typ byte, payload []byte) error { return ErrUnsupported }
func ReadFrame(r io.Reader) (byte, []byte, error)            { return 0, nil, ErrUnsupported }
