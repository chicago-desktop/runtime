// SPDX-License-Identifier: MPL-2.0

package gfx

import (
	"image"
	"image/color"
	"testing"

	"github.com/stretchr/testify/require"
)

// A picture adopted out of a viewport is carried away and encoded later,
// while the producer that drew it keeps running. If the pixels were shared,
// the producer clearing its buffer for the next frame would empty the copy
// too — and the identity still says "unchanged", so the blank is never
// replaced. That is a window arriving as a white rectangle and staying one.
func TestAdoptedPixelsSurviveTheProducerRedrawing(t *testing.T) {
	live := image.NewRGBA(image.Rect(0, 0, 2, 1))
	live.Set(0, 0, color.RGBA{R: 200, G: 30, B: 40, A: 255})

	taken := Adopt(live, 9, 41)
	require.NotNil(t, taken)
	require.Equal(t, uint64(9), taken.Version(), "the identity it arrived with")
	require.Equal(t, uint64(41), taken.Serial())

	// The producer wipes its buffer for the next frame.
	for x := 0; x < 2; x++ {
		live.Set(x, 0, color.RGBA{})
	}

	got := taken.Image().At(0, 0)
	r, g, b, a := got.RGBA()
	require.Equal(t, uint32(200), r>>8, "the red the producer presented")
	require.Equal(t, uint32(30), g>>8)
	require.Equal(t, uint32(40), b>>8)
	require.Equal(t, uint32(255), a>>8)
}
