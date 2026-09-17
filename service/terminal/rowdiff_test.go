// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"bytes"
	"fmt"
	"image/color"
	"math/rand"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
	"github.com/stretchr/testify/require"
	ttyapi "github.com/wippyai/runtime/api/tty"
)

// A screen repainted by changed spans must be the screen repainted whole.
// The judge is a terminal emulator: the frame before, then the spans, against
// the frame alone on a fresh terminal.

const diffCols, diffRows = 40, 6

var diffGraphemes = []string{"a", "b", " ", "ж", "世", "界", "!", " "}

var diffStyles = []uv.Style{
	{},
	{Attrs: uv.AttrBold},
	{Fg: color.RGBA{0xff, 0, 0, 0xff}},
	{Bg: color.RGBA{0, 0, 0x80, 0xff}},
	{Fg: color.RGBA{0xff, 0xff, 0xff, 0xff}, Bg: color.RGBA{0, 0x80, 0x80, 0xff}, Attrs: uv.AttrItalic},
}

// renderRows draws cells as the tty canvas does and renders each row.
func renderRows(cells [][]uv.Cell) []string {
	buffer := canvasWidth{uv.NewBuffer(diffCols, diffRows)}
	for y, line := range cells {
		for x := 0; x < len(line); x++ {
			cell := line[x]
			if cell.IsZero() {
				continue
			}
			buffer.SetCell(x, y, &cell)
		}
	}
	return strings.Split(buffer.Render(), "\n")
}

func randomCells(r *rand.Rand, cells [][]uv.Cell, changes int) {
	for n := 0; n < changes; n++ {
		y, x := r.Intn(diffRows), r.Intn(diffCols-1)
		gr := diffGraphemes[r.Intn(len(diffGraphemes))]
		width := 1
		if gr == "世" || gr == "界" {
			width = 2
		}
		cells[y][x] = uv.Cell{Content: gr, Width: width, Style: diffStyles[r.Intn(len(diffStyles))]}
		// Whatever the new cell covers loses its wide character, and a wide
		// character's orphaned second half becomes a blank: the canvas never
		// holds a continuation without its head.
		end := x + width
		if end < diffCols && cells[y][end].IsZero() {
			cells[y][end] = uv.EmptyCell
		}
		if width == 2 {
			cells[y][x+1] = uv.Cell{}
		}
		// A wide character whose second half was just overwritten is gone.
		if x > 0 && cells[y][x-1].Width == 2 {
			cells[y][x-1] = uv.EmptyCell
		}
	}
}

func screenOf(e *vt.Emulator) string {
	var b strings.Builder
	for y := 0; y < diffRows; y++ {
		for x := 0; x < diffCols; x++ {
			c := e.CellAt(x, y)
			if c == nil {
				b.WriteString("·")
				continue
			}
			fmt.Fprintf(&b, "%q%d%v|", c.Content, c.Width, c.Style.Diff(&uv.Style{}))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func TestSpanRepaintMatchesWholeRepaint(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	cells := make([][]uv.Cell, diffRows)
	for y := range cells {
		cells[y] = make([]uv.Cell, diffCols)
		for x := range cells[y] {
			cells[y][x] = uv.EmptyCell
		}
	}
	randomCells(r, cells, 60)

	var out bytes.Buffer
	surface := NewSurface(&out, ttyapi.SurfaceOptions{})
	live := vt.NewEmulator(diffCols, diffRows)
	_, err := surface.Present(ttyapi.Frame{Rows: renderRows(cells)})
	require.NoError(t, err)
	_, _ = live.Write(out.Bytes())

	partial := 0
	for frame := 0; frame < 300; frame++ {
		randomCells(r, cells, 1+r.Intn(6))
		// Sometimes a row goes blank from the middle to the end.
		if r.Intn(8) == 0 {
			y, from := r.Intn(diffRows), r.Intn(diffCols)
			for x := from; x < diffCols; x++ {
				cells[y][x] = uv.EmptyCell
			}
			if from > 0 && cells[y][from-1].Width == 2 {
				cells[y][from-1] = uv.EmptyCell
			}
		}
		rows := renderRows(cells)

		out.Reset()
		stats, err := surface.Present(ttyapi.Frame{Rows: rows})
		require.NoError(t, err)
		if stats.ChangedRows > 0 && !bytes.Contains(out.Bytes(), []byte("\x1b[2K")) {
			partial++
		}
		_, _ = live.Write(out.Bytes())

		fresh := vt.NewEmulator(diffCols, diffRows)
		var whole bytes.Buffer
		_, err = NewSurface(&whole, ttyapi.SurfaceOptions{}).Present(ttyapi.Frame{Rows: rows})
		require.NoError(t, err)
		_, _ = fresh.Write(whole.Bytes())

		require.Equal(t, screenOf(fresh), screenOf(live), "frame %d differs from a whole repaint", frame)
	}
	require.Greater(t, partial, 200, "the frames must have gone through the span path")
}

// A row that reaches the last column keeps it. The whole-row repaint used to
// erase after writing, which from the pending-wrap position erases the last
// cell.
func TestAFullWidthRowKeepsItsLastColumn(t *testing.T) {
	var out bytes.Buffer
	_, err := NewSurface(&out, ttyapi.SurfaceOptions{}).Present(ttyapi.Frame{Rows: []string{strings.Repeat("x", 9) + "Z"}})
	require.NoError(t, err)
	screen := vt.NewEmulator(10, 2)
	_, _ = screen.Write(out.Bytes())
	require.Equal(t, "Z", screen.CellAt(9, 0).Content)
}

func TestPlainRowAcceptsOnlyWhatCellsKeep(t *testing.T) {
	require.True(t, plainRow("plain"))
	require.True(t, plainRow("\x1b[1;31mred\x1b[m"))
	require.True(t, plainRow("\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\"))
	require.False(t, plainRow("a\x1b[2Kb"), "an erase is not a cell")
	require.False(t, plainRow("a\x1b[5Gb"), "nor is a cursor move")
	require.False(t, plainRow("a\x1b[1;31"), "an unfinished sequence")
	require.False(t, plainRow("\x1b]0;title\x07"), "an OSC other than 8")
}
