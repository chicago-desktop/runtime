// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"os"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
	"github.com/charmbracelet/x/ansi/sixel"
	ttyapi "github.com/wippyai/runtime/api/tty"
)

// Raster placement over the kitty graphics protocol.
//
// Rows and placements are different kinds of thing, and that difference is
// the whole reason this file exists. A row is repainted whenever it changes
// and describes everything on its line. A placement is transmitted once and
// then lives on the terminal's side until someone takes it away — so the
// surface has to remember what it put where, and has to notice when a row
// repaint walked over it.
//
// The protocol itself is encoded by charmbracelet/x/ansi, already a
// dependency: chunking, zlib and the option syntax are its business, not
// ours.

// graphicsProtocol names what the terminal understands. Empty means it
// understands nothing, and that is a refusal with a name rather than a silent
// fallback: an escape sequence a terminal does not know spills onto the
// screen as text and reads as a broken application.
type graphicsProtocol string

const (
	graphicsNone  graphicsProtocol = ttyapi.GraphicsNone
	graphicsKitty graphicsProtocol = ttyapi.GraphicsKitty
	graphicsSixel graphicsProtocol = ttyapi.GraphicsSixel
)

// dirtyRow is written into the remembered frame to force a repaint of a row
// on the next comparison. Sixel has no way to take a picture off the screen
// by name, so the only way to remove one is to paint over the cells it
// occupied — and the surface has to be told those cells are stale.
const dirtyRow = "\x00repaint"

func environmentGraphics() graphicsProtocol {
	protocol, _ := ttyapi.DetectGraphics(os.Getenv)
	return graphicsProtocol(protocol)
}

// probedGraphics is what one terminal can show, by its own answer.
func probedGraphics(probe *ttyapi.Probe) graphicsProtocol {
	protocol, _ := probe.Detect()
	return graphicsProtocol(protocol)
}

// placementState is what the surface remembers about one raster on screen.
type placementState struct {
	serial  uint64
	version uint64
	row     int
	col     int
	cols    int
	rows    int
}

// covers reports whether the placement occupies any cell of the given
// one-based row. A text row repainted under a placement destroys the part of
// the picture sharing those cells, and nothing would notice: for the row
// itself nothing changed.
func (p placementState) covers(row int) bool {
	return row >= p.row && row < p.row+p.rows
}

func (p placementState) sameAs(place ttyapi.Placement) bool {
	// Identity first. A caller that rebuilds a raster every frame restarts
	// its version at one and, drawing it the same way, arrives at the same
	// number — so version alone would call two different pictures the same
	// and leave the older one on screen.
	return p.serial == place.Serial && p.version == place.Version &&
		p.row == place.Row && p.col == place.Col &&
		p.cols == place.Cols && p.rows == place.Rows
}

func stateOf(place ttyapi.Placement) placementState {
	return placementState{
		serial:  place.Serial,
		version: place.Version,
		row:     place.Row,
		col:     place.Col,
		cols:    place.Cols,
		rows:    place.Rows,
	}
}

// graphicsID is the numeric handle the protocol uses. Placements are named
// from Lua, so the name is folded into a stable number; a collision replaces
// one picture with another, which is visible, rather than corrupting the
// stream, which is not.
func graphicsID(name string) int {
	var hash uint32 = 2166136261
	for index := 0; index < len(name); index++ {
		hash ^= uint32(name[index])
		hash *= 16777619
	}
	// The protocol treats 0 as "no id", and the top bit is avoided so the
	// value stays comfortably inside what terminals accept.
	return int(hash&0x7fffffff)&0x7ffffff + 1
}

// appendPlace writes the commands that put one raster on screen.
//
// The cursor is saved and restored around the sequence because the protocol
// places at the cursor: leaving the caret inside a picture would put the next
// text row in the wrong place, and that looks like a defect in whatever drew
// the text.
func appendPlace(out []byte, protocol graphicsProtocol, place ttyapi.Placement) []byte {
	return appendPlaceOn(out, protocol, place, ttyapi.ProcessProbe())
}

// appendPlaceOn is appendPlace for the terminal the probe describes: sixel
// bakes that terminal's cell size into the payload.
func appendPlaceOn(out []byte, protocol graphicsProtocol, place ttyapi.Placement, probe *ttyapi.Probe) []byte {
	if place.Image == nil {
		return out
	}
	if protocol == graphicsSixel {
		return appendSixel(out, place, probe)
	}

	// Raw pixels carry no header, so the transmitted size has to be stated:
	// the encoder does not derive it from the image, and without it the
	// terminal has bytes it cannot interpret. The failure is silent — the
	// sequence looks correct in the stream and nothing appears on screen.
	bounds := place.Image.Bounds()

	var buf bytes.Buffer
	err := kitty.EncodeGraphics(&buf, place.Image, &kitty.Options{
		Action: kitty.TransmitAndPut,
		ID:     graphicsID(place.ID),
		// A named placement, so a later put by id REPLACES it. Without p= every
		// put creates one more placement, and a picture that moved would stay
		// on screen twice.
		PlacementID: kittyPlacement,
		ImageWidth:  bounds.Dx(),
		ImageHeight: bounds.Dy(),
		// Silence the terminal's acknowledgement. Without it the reply
		// arrives on the input path and is decoded as keystrokes: the picture
		// appears and the application starts receiving garbage.
		Quiet:        2,
		Format:       kitty.RGBA,
		Transmission: kitty.Direct,
		Compression:  kitty.Zlib,
		Chunk:        true,
		Columns:      place.Cols,
		Rows:         place.Rows,
		// Cursor restoration cannot undo a scroll caused by placing an image
		// on the last row. Full-screen placements must not advance it.
		DoNotMoveCursor: true,
	})
	if err != nil {
		// A picture that would not encode must not cost the frame. The rest
		// of the screen is still correct, and the caller sees the placement
		// missing rather than losing everything drawn beside it.
		return out
	}

	out = append(out, "\x1b[s"...)
	out = appendCursorTo(out, place.Row, place.Col)
	out = append(out, buf.Bytes()...)
	out = append(out, "\x1b[u"...)
	return out
}

// kittyPlacement is the placement id every raster is put under. One picture
// has one placement on screen, so a constant is enough: what matters is that
// transmit and put name the same one.
const kittyPlacement = 1

// appendPut shows again an image the terminal already holds.
//
// Kitty keeps a transmitted image until it is deleted, so a raster whose
// pixels did not change — only the text row under it was repainted, or it
// moved — needs a put by id, not the full RGBA again. The put names the same
// placement as the transmit, which makes it a replacement, not a second copy.
func appendPut(out []byte, place ttyapi.Placement) []byte {
	options := kitty.Options{
		Action:          kitty.Put,
		ID:              graphicsID(place.ID),
		PlacementID:     kittyPlacement,
		Quiet:           2,
		Columns:         place.Cols,
		Rows:            place.Rows,
		DoNotMoveCursor: true,
	}
	out = append(out, "\x1b[s"...)
	out = appendCursorTo(out, place.Row, place.Col)
	out = append(out, "\x1b_G"...)
	out = append(out, options.String()...)
	out = append(out, '\x1b', '\\')
	out = append(out, "\x1b[u"...)
	return out
}

// defaultEncodedLimit caps the bytes the surface keeps of encoded placements.
// A full 1000×540 screen encodes to a few hundred kilobytes of sixel; the cap
// is there so a long session with many pictures does not grow without end.
const defaultEncodedLimit = 32 << 20

// encodedKey is everything the bytes of one placement command depend on.
//
// Two sends with the same key write the same bytes, so the second is a copy,
// not an encoding. That matters because the encoders cost about 100 ns per
// pixel whatever the picture — 19 ms for a 500×300 window — and a raster that
// did not change but shares a row with repainted text used to pay it again on
// every keystroke. Position is part of the key: screen-origin sixel bakes the
// cell offset into the payload. The cell size is too, for the same reason.
type encodedKey struct {
	protocol  graphicsProtocol
	serial    uint64
	version   uint64
	row       int
	col       int
	cols      int
	rows      int
	cellW     int
	cellH     int
	cellKnown bool
}

func keyOf(protocol graphicsProtocol, place ttyapi.Placement, probe *ttyapi.Probe) encodedKey {
	cellW, cellH, known := probe.CellSize()
	return encodedKey{
		protocol: protocol, serial: place.Serial, version: place.Version,
		row: place.Row, col: place.Col, cols: place.Cols, rows: place.Rows,
		cellW: cellW, cellH: cellH, cellKnown: known,
	}
}

// encodedEntry is the last encoding of one placement.
type encodedEntry struct {
	bytes []byte
	key   encodedKey
	// used is the frame that last wrote these bytes; the oldest go first
	// when the cache is over its limit.
	used uint64
}

// appendDelete removes one raster from the screen by id.
func appendDelete(out []byte, name string) []byte {
	options := kitty.Options{
		Action:          kitty.Delete,
		Delete:          kitty.DeleteID,
		DeleteResources: true,
		Quiet:           2,
		ID:              graphicsID(name),
	}
	out = append(out, "\x1b_G"...)
	out = append(out, options.String()...)
	out = append(out, '\x1b', '\\')
	return out
}

// appendSixel writes one raster as a sixel image.
//
// Sixel has no identifiers: a picture cannot be replaced or removed by name,
// only painted over. That is why removal here means repainting the cells the
// picture occupied, and why the surface remembers its rectangle.
//
// It also has no cell geometry. Kitty scales a raster into a rectangle of
// cells; sixel paints pixels and lets them fall where the cell size puts
// them. The caller's cols and rows therefore describe the damage rectangle,
// not a scale — sizing the raster is the caller's business.
func appendSixel(out []byte, place ttyapi.Placement, probe *ttyapi.Probe) []byte {
	if cw, ch, known := probe.CellSize(); known {
		return appendScreenSixel(out, place, cw, ch)
	}
	var payload bytes.Buffer
	encoder := sixel.Encoder{}
	if err := encoder.Encode(&payload, place.Image); err != nil {
		return out
	}

	out = append(out, "\x1b[s"...)
	out = appendCursorTo(out, place.Row, place.Col)
	// p2 = 1: with 0 every terminal tried leaves a black bar where the
	// background should show through.
	out = append(out, ansi.SixelGraphics(0, 1, 0, payload.Bytes())...)
	out = append(out, "\x1b[u"...)
	return out
}

// A small transparent prefix aligns an image with the six-pixel band grid.
// Encoding it costs at most five additional rows, never a whole screen.
type bandAlignedImage struct {
	image.Image
	padding int
}

func (img bandAlignedImage) Bounds() image.Rectangle {
	bounds := img.Image.Bounds()
	return image.Rect(0, 0, bounds.Dx(), bounds.Dy()+img.padding)
}

func (img bandAlignedImage) At(x, y int) color.Color {
	if y < img.padding {
		return color.Transparent
	}
	bounds := img.Image.Bounds()
	return img.Image.At(bounds.Min.X+x, bounds.Min.Y+y-img.padding)
}

// DECSDM places graphics from the screen origin and clamps at its bottom.
// Cursor-relative sixel instead scrolls when the final six-pixel band extends
// past the bottom, even if the image itself fits. Saving the cursor does not
// undo that scroll. Translate the compact payload to screen coordinates with
// transparent bands/columns, so drawing the taskbar cannot move any text.
func appendScreenSixel(out []byte, place ttyapi.Placement, cellW, cellH int) []byte {
	x, y := max(0, place.Col-1)*cellW, max(0, place.Row-1)*cellH
	aligned := bandAlignedImage{Image: place.Image, padding: y % 6}
	var encoded bytes.Buffer
	if err := (&sixel.Encoder{}).Encode(&encoded, aligned); err != nil {
		return out
	}
	data := encoded.Bytes()
	_, headerLen := sixel.DecodeRaster(data)
	if headerLen == 0 {
		return out
	}
	var payload strings.Builder
	bounds := place.Image.Bounds()
	fmt.Fprintf(&payload, "\"1;1;%d;%d", x+bounds.Dx(), y+bounds.Dy())
	payload.WriteString(strings.Repeat("-", y/6))
	skip := ""
	if x > 0 {
		skip = fmt.Sprintf("!%d?", x)
	}
	payload.WriteString(skip)
	for _, b := range data[headerLen:] {
		payload.WriteByte(b)
		if b == '-' || b == '$' {
			payload.WriteString(skip)
		}
	}
	out = append(out, "\x1b[s\x1b[?80h"...)
	out = append(out, ansi.SixelGraphics(0, 1, 0, []byte(payload.String()))...)
	out = append(out, "\x1b[?80l\x1b[u"...)
	return out
}
