// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"strconv"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Repainting a changed row by the cells that changed, not by the whole row.
//
// A whole-row repaint cost twice: the bytes of the unchanged text, and every
// picture on the row, which the text erased and which then had to be sent
// again. Under a wallpaper that is a full-width strip per keystroke; during a
// window drag, the strips of every row the outline left (app#8).
//
// Rows arrive as rendered strings (the tty canvas renders an ultraviolet
// buffer), so they are read back into cells with the same library and width
// method, compared cell by cell, and only the changed spans are written.

// span is a run of columns, zero-based, end exclusive.
type span struct{ from, to int }

func (s span) overlaps(from, to int) bool { return s.from < to && from < s.to }

// wholeRow is the span of a row repainted from its first column to its end.
var wholeRow = span{0, 1 << 30}

// spanGap is how many unchanged cells between two changes are rewritten
// rather than skipped: a cursor move costs about as much.
const spanGap = 3

// canvasWidth measures cells as the tty canvas does.
type canvasWidth struct{ *uv.Buffer }

func (canvasWidth) WidthMethod() uv.WidthMethod { return ansi.GraphemeWidth }

// cellRow reads a rendered row back into cells, width at least minWidth.
func cellRow(row string, minWidth int) uv.Line {
	width := max(minWidth, ansi.StringWidth(row))
	if width == 0 {
		return uv.Line{}
	}
	buffer := canvasWidth{uv.NewBuffer(width, 1)}
	uv.NewStyledString(row).Draw(buffer, buffer.Bounds())
	return buffer.Line(0)
}

// plainRow reports whether a row holds nothing but text, SGR and OSC 8
// hyperlinks — what reading it back into cells keeps. Anything else (a
// cursor move, a mode) goes through the whole-row repaint as it came.
func plainRow(row string) bool {
	for index := 0; index < len(row); index++ {
		if row[index] != 0x1b {
			continue
		}
		if index+1 >= len(row) {
			return false
		}
		switch row[index+1] {
		case '[':
			end := index + 2
			for end < len(row) && (row[end] < 0x40 || row[end] > 0x7e) {
				end++
			}
			if end >= len(row) || row[end] != 'm' {
				return false
			}
			index = end
		case ']':
			if !strings.HasPrefix(row[index+2:], "8;") {
				return false
			}
			end := strings.Index(row[index:], "\x1b\\")
			bell := strings.IndexByte(row[index:], 0x07)
			if end < 0 && bell < 0 {
				return false
			}
			if end < 0 || (bell >= 0 && bell < end) {
				index += bell
			} else {
				index += end + 1
			}
		default:
			return false
		}
	}
	return true
}

// changedSpans compares two rows cell by cell. forced are spans to repaint
// even where the cells are equal (a picture left them). Spans never cut a
// wide character, and near ones are merged.
func changedSpans(old, cur uv.Line, forced []span) []span {
	width := max(len(old), len(cur))
	var out []span
	at := func(line uv.Line, x int) *uv.Cell {
		if x < len(line) {
			return &line[x]
		}
		return &uv.EmptyCell
	}
	isForced := func(x int) bool {
		for _, f := range forced {
			if x >= f.from && x < f.to {
				return true
			}
		}
		return false
	}
	for x := 0; x < width; x++ {
		if !isForced(x) && at(old, x).Equal(at(cur, x)) {
			continue
		}
		from, to := x, x+1
		// A continuation cell belongs to the wide character before it, in
		// either row.
		for from > 0 && (at(old, from).IsZero() || at(cur, from).IsZero()) {
			from--
		}
		for to < width && (at(old, to).IsZero() || at(cur, to).IsZero()) {
			to++
		}
		if n := len(out); n > 0 && from-out[n-1].to <= spanGap {
			out[n-1].to = max(out[n-1].to, to)
		} else {
			out = append(out, span{from, to})
		}
		x = to - 1
	}
	return out
}

// appendSpan writes the cells of one span at its place on the row, from a
// reset pen and back to one. Unlike ultraviolet's Line.Render it writes
// trailing blanks: they are what erases the old text.
func appendSpan(out []byte, row int, line uv.Line, part span) []byte {
	out = append(out, '\x1b', '[')
	out = strconv.AppendInt(out, int64(row), 10)
	out = append(out, ';')
	out = strconv.AppendInt(out, int64(part.from+1), 10)
	out = append(out, 'H')
	out = append(out, ansi.ResetStyle...)
	var pen uv.Style
	var link uv.Link
	for x := part.from; x < part.to; x++ {
		cell := &uv.EmptyCell
		if x < len(line) {
			cell = &line[x]
		}
		if cell.IsZero() {
			continue
		}
		if !cell.Style.Equal(&pen) {
			if cell.Style.IsZero() {
				out = append(out, ansi.ResetStyle...)
			} else {
				out = append(out, cell.Style.Diff(&pen)...)
			}
			pen = cell.Style
		}
		if cell.Link != link {
			if link.URL != "" {
				out = append(out, ansi.ResetHyperlink()...)
			}
			if cell.Link.URL != "" {
				out = append(out, ansi.SetHyperlink(cell.Link.URL, cell.Link.Params)...)
			}
			link = cell.Link
		}
		content := cell.Content
		if content == "" {
			content = " "
		}
		out = append(out, content...)
	}
	if link.URL != "" {
		out = append(out, ansi.ResetHyperlink()...)
	}
	if !pen.IsZero() {
		out = append(out, ansi.ResetStyle...)
	}
	return out
}
