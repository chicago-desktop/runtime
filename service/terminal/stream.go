// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"time"

	ttyapi "github.com/wippyai/runtime/api/tty"
)

// A terminal that is not this process's own: a person at a remote screen,
// whose keystrokes arrive on a stream and whose screen is written to one.
//
// The file-backed terminal gets three things from the kernel that a stream
// does not have — a size (TIOCGWINSZ), a resize signal (SIGWINCH), and a
// read that can be cancelled (epoll). Each has a replacement here, carried by
// whoever brought the stream: the size and resizes the client reported, and
// a read woken by hand.

// errInputInterrupted is what a read woken by Interrupt returns. It is never
// seen outside: the input reader is being stopped when it happens.
var errInputInterrupted = errors.New("terminal input interrupted")

// streamInput turns a blocking stream into one a reader can be woken from.
//
// One goroutine reads the stream; readers take its chunks from a channel, so
// a read waiting for the next keystroke can be abandoned without abandoning
// the keystroke — it stays in the channel for the next reader.
type streamInput struct {
	chunks   chan []byte
	wake     chan struct{}
	done     chan struct{}
	quit     chan struct{}
	pending  []byte
	quitOnce sync.Once
	mu       sync.Mutex
}

func newStreamInput(stream io.Reader) *streamInput {
	s := &streamInput{
		chunks: make(chan []byte, 64),
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
		quit:   make(chan struct{}),
	}
	go s.pump(stream)
	return s
}

func (s *streamInput) pump(stream io.Reader) {
	defer close(s.done)
	buf := make([]byte, 4096)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			select {
			case s.chunks <- append([]byte(nil), buf[:n]...):
			case <-s.quit:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// close lets the pump go when nobody will read again. The stream itself is
// closed by its owner; this only stops a pump blocked on handing over a
// chunk that no program is left to take.
func (s *streamInput) close() {
	s.quitOnce.Do(func() { close(s.quit) })
}

// Read returns what the stream delivered, io.EOF once it ended and every
// chunk was taken, and errInputInterrupted when woken with nothing to read.
func (s *streamInput) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) > 0 {
		return s.takePending(p), nil
	}
	select {
	case chunk := <-s.chunks:
		return s.take(p, chunk), nil
	case <-s.wake:
		return 0, errInputInterrupted
	case <-s.done:
		// The stream ended, but chunks sent before the end may still be
		// waiting: the pump closes done right after its last send.
		select {
		case chunk := <-s.chunks:
			return s.take(p, chunk), nil
		default:
			return 0, io.EOF
		}
	}
}

// Interrupt wakes one Read blocked on the stream.
func (s *streamInput) Interrupt() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// readProbe collects a terminal's answer to probeQuery: every byte until the
// Primary Device Attributes reply that closes it, or until the deadline.
//
// What arrived after the answer is kept for the program — unlike the
// process's own terminal, a stream can hold on to bytes. What arrived before
// a missing answer cannot be told apart from it and is dropped, as it is
// there.
func (s *streamInput) readProbe(timeout time.Duration) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	collected := append([]byte(nil), s.pending...)
	s.pending = nil
	for {
		if end, closed := deviceAttributesEnd(collected); closed {
			s.pending = append(s.pending, collected[end:]...)
			return collected[:end], true
		}
		select {
		case chunk := <-s.chunks:
			collected = append(collected, chunk...)
		case <-s.done:
			select {
			case chunk := <-s.chunks:
				collected = append(collected, chunk...)
				continue
			default:
			}
			return collected, len(collected) > 0
		case <-timer.C:
			return collected, len(collected) > 0
		}
	}
}

func (s *streamInput) take(p, chunk []byte) int {
	n := copy(p, chunk)
	if n < len(chunk) {
		s.pending = append(s.pending, chunk[n:]...)
	}
	return n
}

func (s *streamInput) takePending(p []byte) int {
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n
}

// deviceAttributesEnd returns the index just past the first complete Primary
// Device Attributes reply, by the same rules findDeviceAttributes reads it.
func deviceAttributesEnd(reply []byte) (int, bool) {
	offset := 0
	for {
		start := bytes.Index(reply[offset:], []byte("\x1b[?"))
		if start < 0 {
			return 0, false
		}
		bodyStart := offset + start + 3
		end := bytes.IndexByte(reply[bodyStart:], 'c')
		if end < 0 {
			return 0, false
		}
		if bytes.IndexByte(reply[bodyStart:bodyStart+end], '\x1b') >= 0 {
			offset = bodyStart
			continue
		}
		return bodyStart + end + 1, true
	}
}

// sessionTerminal is one remote screen.
type sessionTerminal struct {
	input   *streamInput
	probe   *ttyapi.Probe
	resized chan struct{}
	term    string
	mu      sync.Mutex
	cols    int
	rows    int
	pixelW  int
	pixelH  int
}

// newSessionTerminal reads keystrokes from stream. env is what the client
// said about its own terminal; the guesses of the probe read that and never
// the server's environment.
func newSessionTerminal(stream io.Reader, termType string, env map[string]string) *sessionTerminal {
	vars := make(map[string]string, len(env)+1)
	for name, value := range env {
		vars[name] = value
	}
	if termType != "" {
		vars["TERM"] = termType
	}
	return &sessionTerminal{
		input:   newStreamInput(stream),
		probe:   ttyapi.NewProbe(func(name string) string { return vars[name] }),
		resized: make(chan struct{}, 1),
		term:    termType,
	}
}

// close releases the stream's reader once no program is left to read.
func (t *sessionTerminal) close() { t.input.close() }

func (t *sessionTerminal) Read(p []byte) (int, error) { return t.input.Read(p) }
func (t *sessionTerminal) Interrupt()                 { t.input.Interrupt() }
func (t *sessionTerminal) Resized() <-chan struct{}   { return t.resized }
func (t *sessionTerminal) TermType() string           { return t.term }

// Size reports the screen as the client last described it. Pixel dimensions,
// when the client sends them, refresh the cell size the way a font zoom does
// on the process's own terminal.
func (t *sessionTerminal) Size() (int, int, error) {
	t.mu.Lock()
	cols, rows, pixelW, pixelH := t.cols, t.rows, t.pixelW, t.pixelH
	t.mu.Unlock()
	if cols <= 0 || rows <= 0 {
		return 0, 0, errors.New("the client did not report its terminal size")
	}
	if pixelW > 0 && pixelH > 0 {
		t.probe.SetCellSize(pixelW/cols, pixelH/rows)
	}
	return cols, rows, nil
}

// resize records a new size and tells whoever is listening. A resize that
// finds one already waiting is folded into it: the listener reads the size
// when it wakes, not the one that woke it.
func (t *sessionTerminal) resize(cols, rows, pixelW, pixelH int) {
	t.mu.Lock()
	t.cols, t.rows, t.pixelW, t.pixelH = cols, rows, pixelW, pixelH
	t.mu.Unlock()
	select {
	case t.resized <- struct{}{}:
	default:
	}
}

// ask puts probeQuery to the remote terminal and records its answer.
//
// It runs before the program does, for the same reason as on the process's
// own terminal: the answer comes back on the input path, and from the moment
// the program starts that path is the program's.
func (t *sessionTerminal) ask(out io.Writer, timeout time.Duration) {
	if _, err := out.Write([]byte(probeQuery)); err != nil {
		return
	}
	reply, ok := t.input.readProbe(timeout)
	if !ok {
		t.probe.SetSilence()
		return
	}
	if width, height, ok := parseCellSize(reply); ok {
		t.probe.SetCellSize(width, height)
	} else if width, height, ok := parseTextArea(reply); ok {
		t.mu.Lock()
		cols, rows := t.cols, t.rows
		t.mu.Unlock()
		if cols > 0 && rows > 0 {
			t.probe.SetCellSize(width/cols, height/rows)
		}
	}
	protocol, reason, answered := parseProbeReply(reply)
	if !answered {
		t.probe.SetSilence()
		return
	}
	t.probe.SetGraphics(protocol, reason)
}

// onlcrWriter does what a pty's ONLCR does for the process's own terminal:
// a line feed becomes carriage return plus line feed. Without a pty on this
// side nobody else would, and a program that prints lines would draw them as
// a staircase on the remote screen.
//
// It also serializes writers: the surface and the input reader (mouse and
// paste modes) write from different goroutines.
type onlcrWriter struct {
	w   io.Writer
	buf []byte
	mu  sync.Mutex
}

func (o *onlcrWriter) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if bytes.IndexByte(p, '\n') < 0 {
		return o.w.Write(p)
	}
	o.buf = o.buf[:0]
	for _, b := range p {
		if b == '\n' {
			o.buf = append(o.buf, '\r')
		}
		o.buf = append(o.buf, b)
	}
	written, err := o.w.Write(o.buf)
	if err != nil {
		return 0, err
	}
	if written != len(o.buf) {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}

// sessionRaw is the raw-mode control of a remote terminal. The client put its
// terminal into raw mode when it asked for a pty, so there is nothing to do —
// but a program that asks must hear yes, not "unavailable".
type sessionRaw struct {
	mu   sync.Mutex
	refs int
}

func (r *sessionRaw) Enable() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs++
	return nil
}

func (r *sessionRaw) Disable() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refs > 0 {
		r.refs--
	}
	return nil
}

func (r *sessionRaw) Reset() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = 0
	return nil
}

func (r *sessionRaw) Enabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refs > 0
}

var (
	_ inputSource          = (*sessionTerminal)(nil)
	_ ttyapi.RawController = (*sessionRaw)(nil)
)
