// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	ttyapi "github.com/wippyai/runtime/api/tty"
)

// sentOrder lists the placements an encoder was asked for, in order.
func sentOrder(surface *Surface) *[]string {
	order := &[]string{}
	surface.encodeSixel = func(place ttyapi.Placement, pad int) []byte {
		*order = append(*order, place.ID)
		return sixelPayload(place.Image, pad)
	}
	return order
}

// A wallpaper strip is redrawn (a new wallpaper) in place while the widget
// standing on the same row is not. Sixel has no layers, so the strip sent
// alone would lie over the widget: the widget has to go out again after it.
// An icon elsewhere on the row, not under the strip's new pixels, does not.
func TestSixelResendsWhatAChangedPictureWouldCover(t *testing.T) {
	var out bytes.Buffer
	surface := NewSurface(&out, ttyapi.SurfaceOptions{})
	surface.graphics = graphicsSixel
	order := sentOrder(surface)
	img := fullPalette()
	rows := []string{"", "", "", ""}
	frame := func(stripVersion uint64) []ttyapi.Placement {
		return []ttyapi.Placement{
			{ID: "desk:pattern:2", Image: img, Serial: 1, Version: stripVersion, Row: 2, Col: 1, Cols: 30, Rows: 1},
			{ID: "widget:clock:row:1", Image: img, Serial: 2, Version: 1, Row: 2, Col: 20, Cols: 8, Rows: 2},
			{ID: "desk:icon", Image: img, Serial: 3, Version: 1, Row: 3, Col: 1, Cols: 4, Rows: 1},
		}
	}

	_, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: frame(1)})
	require.NoError(t, err)
	require.Equal(t, []string{"desk:pattern:2", "widget:clock:row:1", "desk:icon"}, *order)

	*order = nil
	out.Reset()
	stats, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: frame(2)})
	require.NoError(t, err)
	require.Equal(t, []string{"desk:pattern:2"}, *order, "only the strip is encoded; the widget is a cached copy")
	require.Equal(t, 2, stats.PlacementsSent, "the strip, then the widget it would have covered")
	require.Equal(t, 2, bytes.Count(out.Bytes(), []byte("\x1bP0;1q")), "the icon on row 3 is not under the strip")
}

// Order matters the other way too: a changed picture HIGHER in the stack
// does not drag the ones under it along.
func TestSixelDoesNotResendWhatLiesBelow(t *testing.T) {
	var out bytes.Buffer
	surface := NewSurface(&out, ttyapi.SurfaceOptions{})
	surface.graphics = graphicsSixel
	img := fullPalette()
	rows := []string{"", "", ""}
	frame := func(topVersion uint64) []ttyapi.Placement {
		return []ttyapi.Placement{
			{ID: "desk:pattern:2", Image: img, Serial: 1, Version: 1, Row: 2, Col: 1, Cols: 30, Rows: 1},
			{ID: "win:1:head", Image: img, Serial: 2, Version: topVersion, Row: 2, Col: 5, Cols: 10, Rows: 1},
		}
	}
	_, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: frame(1)})
	require.NoError(t, err)
	stats, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: frame(2)})
	require.NoError(t, err)
	require.Equal(t, 1, stats.PlacementsSent)
}
