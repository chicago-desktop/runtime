// SPDX-License-Identifier: MPL-2.0

package tty

import uv "github.com/charmbracelet/ultraviolet"

// canvasRegion enforces placement bounds at the cell-write boundary, not just
// at the styled-string decoder. A canvas accepts styled cells, not terminal
// commands: control-only decoder output must never enter its rendered rows.
type canvasRegion struct {
	*canvasBuffer
	area     uv.Rectangle
	defaults uv.Style
}

func (r *canvasRegion) Bounds() uv.Rectangle { return r.area }

func (r *canvasRegion) CellAt(x, y int) *uv.Cell {
	if !uv.Pos(x, y).In(r.area) {
		return nil
	}
	return r.canvasBuffer.CellAt(x, y)
}

func (r *canvasRegion) SetCell(x, y int, cell *uv.Cell) {
	if !uv.Pos(x, y).In(r.area) || (cell != nil && cell.Width <= 0) {
		return
	}
	if cell != nil && ((cell.Style.Fg == nil && r.defaults.Fg != nil) || (cell.Style.Bg == nil && r.defaults.Bg != nil)) {
		copy := *cell
		if copy.Style.Fg == nil {
			copy.Style.Fg = r.defaults.Fg
		}
		if copy.Style.Bg == nil {
			copy.Style.Bg = r.defaults.Bg
		}
		cell = &copy
	}
	if cell != nil && x+cell.Width > r.area.Max.X {
		blank := *cell
		blank.Empty()
		for column := x; column < r.area.Max.X; column++ {
			r.canvasBuffer.SetCell(column, y, &blank)
		}
		return
	}
	r.canvasBuffer.SetCell(x, y, cell)
}
