// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"bytes"
	"image"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/pid"
	ttyapi "github.com/wippyai/runtime/api/tty"
)

// sixelReply is a terminal saying: a cell is 10x20, and I speak sixel.
const sixelReply = "\x1b[6;20;10t\x1b[?62;4c"

func pipeInput(t *testing.T) (*streamInput, *io.PipeWriter) {
	t.Helper()
	reader, writer := io.Pipe()
	input := newStreamInput(reader)
	t.Cleanup(func() {
		_ = writer.Close()
		input.close()
	})
	return input, writer
}

func TestStreamInputKeepsWhatFollowsTheProbeAnswer(t *testing.T) {
	input, writer := pipeInput(t)
	go func() { _, _ = writer.Write([]byte(sixelReply + "ab")) }()

	reply, ok := input.readProbe(time.Second)
	require.True(t, ok)
	assert.Equal(t, sixelReply, string(reply))

	buf := make([]byte, 8)
	n, err := input.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "ab", string(buf[:n]), "keys typed after the answer belong to the program")
}

func TestStreamInputInterruptWakesAReadWithoutLosingAKey(t *testing.T) {
	input, writer := pipeInput(t)
	result := make(chan error, 1)
	go func() {
		_, err := input.Read(make([]byte, 8))
		result <- err
	}()
	input.Interrupt()
	select {
	case err := <-result:
		require.ErrorIs(t, err, errInputInterrupted)
	case <-time.After(time.Second):
		t.Fatal("Interrupt did not wake a read blocked on the stream")
	}

	go func() { _, _ = writer.Write([]byte("x")) }()
	buf := make([]byte, 8)
	n, err := input.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "x", string(buf[:n]))
}

func TestStreamInputEndsAfterTheLastChunk(t *testing.T) {
	input, writer := pipeInput(t)
	go func() {
		_, _ = writer.Write([]byte("z"))
		_ = writer.Close()
	}()
	buf := make([]byte, 8)
	n, err := input.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "z", string(buf[:n]))
	_, err = input.Read(buf)
	assert.ErrorIs(t, err, io.EOF)
}

func askingTerminal(t *testing.T, answer string, cols, rows int) (*sessionTerminal, *bytes.Buffer) {
	t.Helper()
	reader, writer := io.Pipe()
	terminal := newSessionTerminal(reader, "xterm-256color", nil)
	terminal.cols, terminal.rows = cols, rows
	t.Cleanup(func() {
		_ = writer.Close()
		terminal.close()
	})
	if answer != "" {
		go func() { _, _ = writer.Write([]byte(answer)) }()
	}
	return terminal, &bytes.Buffer{}
}

func TestSessionTerminalRecordsItsOwnAnswer(t *testing.T) {
	ttyapi.ForgetProbedGraphics()
	t.Cleanup(ttyapi.ForgetProbedGraphics)
	terminal, out := askingTerminal(t, sixelReply, 100, 30)

	terminal.ask(out, time.Second)

	assert.Equal(t, probeQuery, out.String(), "the question goes to the remote screen")
	width, height, known := terminal.probe.CellSize()
	assert.True(t, known)
	assert.Equal(t, [2]int{10, 20}, [2]int{width, height})
	protocol, _ := terminal.probe.Detect()
	assert.Equal(t, ttyapi.GraphicsSixel, protocol)
	_, _, processKnows := ttyapi.CellSize()
	assert.False(t, processKnows, "a session's answer must not become the server's")
}

func TestSessionTerminalFallsBackToTheTextArea(t *testing.T) {
	terminal, out := askingTerminal(t, "\x1b[4;600;1000t\x1b[?62c", 100, 30)

	terminal.ask(out, time.Second)

	width, height, known := terminal.probe.CellSize()
	assert.True(t, known)
	assert.Equal(t, [2]int{10, 20}, [2]int{width, height})
	protocol, reason := terminal.probe.Detect()
	assert.Equal(t, ttyapi.GraphicsNone, protocol)
	assert.Contains(t, reason, "answered with no graphics protocol")
}

func TestSessionTerminalThatSaysNothing(t *testing.T) {
	terminal, out := askingTerminal(t, "", 100, 30)

	terminal.ask(out, 30*time.Millisecond)

	protocol, reason := terminal.probe.Detect()
	assert.Equal(t, ttyapi.GraphicsNone, protocol)
	assert.Contains(t, reason, "did not answer")
}

func TestOnlcrWriterTranslatesLineFeeds(t *testing.T) {
	var out bytes.Buffer
	writer := &onlcrWriter{w: &out}

	n, err := writer.Write([]byte("a\nb"))
	require.NoError(t, err)
	assert.Equal(t, 3, n, "the caller's count, not the translated one")
	assert.Equal(t, "a\r\nb", out.String())

	out.Reset()
	_, _ = writer.Write([]byte("\x1b[1;1Hplain"))
	assert.Equal(t, "\x1b[1;1Hplain", out.String())
}

func TestProbedSurfaceDrawsForItsOwnTerminal(t *testing.T) {
	// The server's own terminal: sixel with 10x20 cells.
	ttyapi.SetProbedGraphics(ttyapi.GraphicsSixel, "")
	ttyapi.SetProbedCellSize(10, 20)
	t.Cleanup(ttyapi.ForgetProbedGraphics)
	// The session's: sixel with 8x16.
	probe := ttyapi.NewProbe(nil)
	probe.SetGraphics(ttyapi.GraphicsSixel, "")
	probe.SetCellSize(8, 16)

	var out bytes.Buffer
	surface := NewProbedSurface(&out, ttyapi.SurfaceOptions{}, probe)
	_, err := surface.Present(ttyapi.Frame{
		Rows: []string{"", "", ""},
		Placements: []ttyapi.Placement{{
			Image: image.NewRGBA(image.Rect(0, 0, 8, 16)),
			ID:    "icon", Version: 1, Serial: 1, Row: 2, Col: 3, Cols: 1, Rows: 1,
		}},
	})
	require.NoError(t, err)

	// Screen-origin sixel skips the columns left of the picture in pixels:
	// two cells of this terminal are 16, of the server's would be 20.
	assert.Contains(t, out.String(), "!16?")
	assert.NotContains(t, out.String(), "!20?")
}

func TestStreamInputReaderFollowsTheRemoteTerminal(t *testing.T) {
	reader, writer := io.Pipe()
	terminal := newSessionTerminal(reader, "xterm-256color", nil)
	terminal.cols, terminal.rows = 100, 30
	t.Cleanup(func() {
		_ = writer.Close()
		terminal.close()
	})
	events := make(chan *TTYEvent, 16)
	input := newStreamInputReader(terminal, io.Discard, nil, pid.PID{})
	input.deliver = func(event *TTYEvent) { events <- event }
	require.NoError(t, input.Start())

	next := func() *TTYEvent {
		t.Helper()
		select {
		case event := <-events:
			return event
		case <-time.After(2 * time.Second):
			t.Fatal("no event from the remote terminal")
			return nil
		}
	}

	start := next()
	assert.Equal(t, "start", start.Type)
	assert.Equal(t, [2]int{100, 30}, [2]int{start.Width, start.Height})

	go func() { _, _ = writer.Write([]byte("q")) }()
	key := next()
	assert.Equal(t, "key", key.Type)
	assert.Equal(t, "q", key.Key)

	terminal.resize(120, 40, 1200, 800)
	resized := next()
	assert.Equal(t, "resize", resized.Type)
	assert.Equal(t, [2]int{120, 40}, [2]int{resized.Width, resized.Height})
	width, height, _ := terminal.probe.CellSize()
	assert.Equal(t, [2]int{10, 20}, [2]int{width, height}, "reported pixels refresh the cell size")

	stopped := make(chan error, 1)
	go func() { stopped <- input.Stop() }()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Stop hung on a read nobody woke")
	}
}
