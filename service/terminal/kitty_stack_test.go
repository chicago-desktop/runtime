// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"bytes"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	ttyapi "github.com/wippyai/runtime/api/tty"
)

var kittyCommand = regexp.MustCompile(`\x1b_G([^;\x1b]*)`)

// zOf returns the z= of each kitty command in the output, by image id.
func zOf(t *testing.T, out []byte) map[string]string {
	t.Helper()
	found := map[string]string{}
	for _, m := range kittyCommand.FindAllSubmatch(out, -1) {
		id, z := "", ""
		for _, kv := range bytes.Split(m[1], []byte(",")) {
			switch {
			case bytes.HasPrefix(kv, []byte("i=")):
				id = string(kv[2:])
			case bytes.HasPrefix(kv, []byte("z=")):
				z = string(kv[2:])
			}
		}
		if id != "" && z != "" {
			found[id] = z
		}
	}
	return found
}

func idOf(name string) string { return strconv.Itoa(graphicsID(name)) }

// Kitty stacks pictures of equal z by image id, and ids here are hashes of
// names — random with respect to the frame. The frame order is the stack, so
// every placement carries z by its place in the list.
func TestKittyStacksPlacementsInFrameOrder(t *testing.T) {
	var out bytes.Buffer
	surface := NewSurface(&out, ttyapi.SurfaceOptions{})
	surface.graphics = graphicsKitty
	img := fullPalette()
	strip := ttyapi.Placement{ID: "desk:pattern:2", Image: img, Serial: 1, Version: 1, Row: 2, Col: 1, Cols: 30, Rows: 1}
	widget := ttyapi.Placement{ID: "widget:clock:row:1", Image: img, Serial: 2, Version: 1, Row: 2, Col: 20, Cols: 8, Rows: 2}
	outline := ttyapi.Placement{ID: "outline:top", Image: img, Serial: 3, Version: 1, Row: 1, Col: 1, Cols: 30, Rows: 1}
	rows := []string{"", "", ""}

	_, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: []ttyapi.Placement{strip, widget, outline}})
	require.NoError(t, err)
	require.Equal(t, map[string]string{idOf(strip.ID): "1", idOf(widget.ID): "2", idOf(outline.ID): "3"}, zOf(t, out.Bytes()))

	// The widget leaves the frame: the outline moves down the stack, and a
	// put by id with its new z is all it needs.
	out.Reset()
	stats, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: []ttyapi.Placement{strip, outline}})
	require.NoError(t, err)
	require.Equal(t, 1, stats.PlacementsSent)
	require.Equal(t, map[string]string{idOf(outline.ID): "2"}, zOf(t, out.Bytes()))
	require.Contains(t, out.String(), "a=p", "a new z is a put, not a transmit")
}
