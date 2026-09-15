// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/charmbracelet/x/input"
	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	"github.com/wippyai/runtime/api/payload"
	"github.com/wippyai/runtime/api/pid"
	"github.com/wippyai/runtime/api/relay"
	ttyapi "github.com/wippyai/runtime/api/tty"
	"github.com/wippyai/runtime/system/scheduler/actor"
)

// inputSource is a terminal that is not a file on this machine: a remote
// session whose bytes arrive on a stream.
//
// Everything the file-backed reader asks the kernel, this one asks the
// stream's carrier instead — the size comes from what the client reported,
// a resize from its window-change message rather than SIGWINCH, and a read
// blocked on the stream has to be woken by hand, because nothing can poll it.
type inputSource interface {
	io.Reader
	// TermType is the client's TERM; the process's own describes the server.
	TermType() string
	// Size is the screen in cells as the client last reported it.
	Size() (int, int, error)
	// Resized signals that Size changed.
	Resized() <-chan struct{}
	// Interrupt wakes a Read blocked on the stream without consuming data.
	Interrupt()
}

// InputReader reads terminal input and delivers parsed events via the scheduler.
type InputReader struct {
	output    io.Writer
	emitter   *inputEmitter
	raw       ttyapi.RawController
	scheduler *actor.Scheduler
	reader    *input.Reader
	cancel    context.CancelFunc
	stopDone  chan struct{}
	stopErr   error
	stdin     *os.File
	source    inputSource
	// deliver replaces the scheduler as the destination of events. Tests
	// use it; nil means the target process.
	deliver      func(*TTYEvent)
	targetPID    pid.PID
	wg           sync.WaitGroup
	mu           sync.Mutex
	started      bool
	stopping     bool
	mouseEnabled bool
	pasteEnabled bool
}

// NewInputReader creates an InputReader that delivers events to the given process.
func NewInputReader(stdin *os.File, output io.Writer, raw *RawManager, scheduler *actor.Scheduler, targetPID pid.PID) *InputReader {
	if output == nil {
		output = io.Discard
	}
	r := &InputReader{
		stdin:     stdin,
		output:    output,
		scheduler: scheduler,
		targetPID: targetPID,
	}
	if raw != nil {
		r.raw = raw
	}
	return r
}

// newStreamInputReader reads a remote terminal. There is no raw mode to set
// on this side: the client put its own terminal into raw mode when it asked
// for a pty, and the bytes arrive exactly as typed.
func newStreamInputReader(source inputSource, output io.Writer, scheduler *actor.Scheduler, targetPID pid.PID) *InputReader {
	if output == nil {
		output = io.Discard
	}
	return &InputReader{
		source:    source,
		output:    output,
		scheduler: scheduler,
		targetPID: targetPID,
	}
}

// Start enables raw mode and spawns the read loop and the resize goroutine.
func (r *InputReader) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.started || r.stopping {
		return errors.New("input reader already started")
	}
	r.stopErr = nil

	if err := r.enableRaw(); err != nil {
		return err
	}

	var reader *input.Reader
	var err error
	if r.source != nil {
		reader, err = input.NewReader(r.source, r.source.TermType(), 0)
	} else {
		reader, err = input.NewReader(r.stdin, os.Getenv("TERM"), 0)
	}
	if err != nil {
		_ = r.disableRaw()
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.reader = reader
	r.emitter = newInputEmitter(r.sendNow)
	r.started = true
	pasteSequence := []byte("\033[?2004h")
	if n, err := r.output.Write(pasteSequence); err != nil || n != len(pasteSequence) {
		r.started = false
		cancel()
		_ = reader.Close()
		_ = r.disableRaw()
		if err == nil {
			err = io.ErrShortWrite
		}
		return err
	}
	r.pasteEnabled = true

	// Send initial start event with terminal size
	cols, rows, sizeErr := r.screenSize()
	if sizeErr == nil {
		r.emitter.emit(&TTYEvent{
			Type:   "start",
			Width:  cols,
			Height: rows,
		})
	}

	r.wg.Add(2)
	go r.readLoop(ctx, reader)
	if r.source != nil {
		go r.resizeLoop(ctx, r.source.Resized())
	} else {
		go r.sigwinchLoop(ctx)
	}

	return nil
}

// Stop cancels the read loop, waits for goroutines, and restores the terminal.
func (r *InputReader) Stop() error {
	r.mu.Lock()
	if r.stopping {
		done := r.stopDone
		r.mu.Unlock()
		<-done
		r.mu.Lock()
		err := r.stopErr
		r.mu.Unlock()
		return err
	}
	if !r.started {
		r.mu.Unlock()
		return nil
	}

	r.started = false
	r.stopping = true
	r.stopDone = make(chan struct{})
	if r.emitter != nil {
		r.emitter.stop()
	}
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	if r.reader != nil {
		r.reader.Cancel()
	}
	// A stream cannot be cancelled from the outside the way a file can: the
	// read loop is blocked in Read until a byte arrives, and the next byte
	// may never come. Waking it is the only way the wait below ends.
	if r.source != nil {
		r.source.Interrupt()
	}
	r.mu.Unlock()

	r.wg.Wait()

	r.mu.Lock()
	if r.reader != nil {
		_ = r.reader.Close()
		r.reader = nil
	}

	// Disable mouse tracking if it was enabled
	if r.mouseEnabled {
		_, _ = r.output.Write([]byte("\033[?1006l\033[?1003l"))
		r.mouseEnabled = false
	}
	var pasteErr error
	if r.pasteEnabled {
		sequence := []byte("\033[?2004l")
		if n, err := r.output.Write(sequence); err != nil {
			pasteErr = err
		} else if n != len(sequence) {
			pasteErr = io.ErrShortWrite
		}
		r.pasteEnabled = false
	}
	r.stopping = false
	r.stopErr = errors.Join(pasteErr, r.disableRaw())
	close(r.stopDone)
	r.stopDone = nil
	err := r.stopErr
	r.mu.Unlock()
	return err
}

func (r *InputReader) enableRaw() error {
	if r.raw == nil {
		return nil
	}
	return r.raw.Enable()
}

func (r *InputReader) disableRaw() error {
	if r.raw == nil {
		return nil
	}
	return r.raw.Disable()
}

// EnableMouse enables mouse event tracking (SGR mode).
func (r *InputReader) EnableMouse() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started && !r.stopping && !r.mouseEnabled {
		_, _ = r.output.Write([]byte("\033[?1003h\033[?1006h"))
		r.mouseEnabled = true
	}
}

// DisableMouse disables mouse event tracking.
func (r *InputReader) DisableMouse() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mouseEnabled {
		_, _ = r.output.Write([]byte("\033[?1006l\033[?1003l"))
		r.mouseEnabled = false
	}
}

// ScreenSize returns the current terminal dimensions.
func (r *InputReader) ScreenSize() (int, int, error) {
	return r.screenSize()
}

func (r *InputReader) screenSize() (int, int, error) {
	if r.source != nil {
		return r.source.Size()
	}
	// A font zoom can change cell pixels even when rows and columns stay
	// the same. Refresh before delivering resize, so gfx and the compositor
	// rebuild their rasters at the new native size. This ioctl does not read
	// input or inject terminal queries into a running application.
	size, err := pty.GetsizeFull(r.stdin)
	if err != nil {
		return term.GetSize(r.stdin.Fd())
	}
	cols, rows := int(size.Cols), int(size.Rows)
	if cols > 0 && rows > 0 && size.X > 0 && size.Y > 0 {
		ttyapi.SetProbedCellSize(int(size.X)/cols, int(size.Y)/rows)
	}
	return cols, rows, nil
}

func (r *InputReader) readLoop(ctx context.Context, reader *input.Reader) {
	defer r.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		events, err := reader.ReadEvents()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}

		for _, ev := range events {
			ttyEv := ConvertInputEvent(ev)
			if ttyEv != nil {
				r.sendEvent(ttyEv)
			}
		}
	}
}

// resizeLoop is sigwinchLoop for a remote terminal: its resizes arrive as
// messages from the client, not as a signal to this process.
func (r *InputReader) resizeLoop(ctx context.Context, resized <-chan struct{}) {
	defer r.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-resized:
			r.emitResize()
		}
	}
}

func (r *InputReader) emitResize() {
	cols, rows, err := r.screenSize()
	if err == nil {
		r.sendEvent(&TTYEvent{
			Type:   "resize",
			Width:  cols,
			Height: rows,
		})
	}
}

func (r *InputReader) sendEvent(ev *TTYEvent) {
	r.mu.Lock()
	emitter := r.emitter
	r.mu.Unlock()
	if emitter != nil {
		emitter.emit(ev)
	}
}

func (r *InputReader) sendNow(ev *TTYEvent) {
	if r.deliver != nil {
		r.deliver(ev)
		return
	}
	pkg := relay.AcquirePackage()
	pkg.Target = r.targetPID
	pkg.AddMessage(relay.Topic(TopicTTYEvents), payload.New(ev))
	if err := r.scheduler.Send(pkg); err != nil {
		relay.ReleasePackage(pkg)
	}
}

var _ ttyapi.InputController = (*InputReader)(nil)
