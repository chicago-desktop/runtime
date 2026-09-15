// SPDX-License-Identifier: MPL-2.0

//go:build !windows

package terminal

import (
	"io"
	"testing"

	"github.com/creack/pty"
	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/pid"
	ttyapi "github.com/wippyai/runtime/api/tty"
)

func TestResizeRefreshesCellPixelsBeforeDelivery(t *testing.T) {
	master, slave, err := pty.Open()
	require.NoError(t, err)
	defer master.Close()
	defer slave.Close()
	ttyapi.ForgetProbedGraphics()
	t.Cleanup(ttyapi.ForgetProbedGraphics)
	reader := NewInputReader(slave, io.Discard, NewRawManager(slave), nil, pid.PID{})
	var received *TTYEvent
	var cellW, cellH int
	var known bool
	reader.emitter = newInputEmitter(func(event *TTYEvent) {
		received = event
		cellW, cellH, known = ttyapi.CellSize()
	})
	for _, size := range []struct{ w, h uint16 }{{10, 20}, {8, 18}, {12, 24}} {
		require.NoError(t, pty.Setsize(master, &pty.Winsize{
			Cols: 80, Rows: 24, X: 80 * size.w, Y: 24 * size.h,
		}))
		received = nil
		reader.emitResize()
		require.NotNil(t, received)
		require.Equal(t, "resize", received.Type)
		require.Equal(t, 80, received.Width)
		require.Equal(t, 24, received.Height)
		require.True(t, known)
		require.Equal(t, int(size.w), cellW)
		require.Equal(t, int(size.h), cellH)
	}
	// Some terminals/SSH peers omit pixels. Preserve the probed value;
	// never replace it with a guessed 8x16 or divide by a zero cell count.
	for _, size := range []*pty.Winsize{
		{Cols: 100, Rows: 30}, {Cols: 0, Rows: 0, X: 800, Y: 600},
	} {
		require.NoError(t, pty.Setsize(master, size))
		_, _, err := reader.ScreenSize()
		require.NoError(t, err)
		w, h, ok := ttyapi.CellSize()
		require.True(t, ok)
		require.Equal(t, 12, w)
		require.Equal(t, 24, h)
	}
}
