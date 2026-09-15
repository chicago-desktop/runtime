// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"fmt"
	"io"
	"strconv"
	"sync"

	ttyapi "github.com/wippyai/runtime/api/tty"
)

// Surface is the physical ANSI implementation of tty.Surface.
type Surface struct {
	out      io.Writer
	closeErr error
	cursor   *ttyapi.Cursor
	// What rasters are on screen and where. They survive between frames on
	// the terminal's side, so the surface is the only thing that knows they
	// exist — nobody else can take them away.
	placements map[string]placementState
	// The last encoding of each placement, by id. A raster sent again with
	// the same key (see encodedKey) is copied from here instead of encoded.
	encoded      map[string]encodedEntry
	encodedBytes int
	// encodedLimit caps encodedBytes; zero turns the cache off.
	encodedLimit int
	// encode writes the command that places one raster. Tests replace it to
	// count encodings; nil means appendPlaceOn with the surface's probe.
	encode   func(out []byte, protocol graphicsProtocol, place ttyapi.Placement) []byte
	frameSeq uint64
	rows     []string
	scratch  []byte
	graphics graphicsProtocol
	// probe is the terminal this surface presents to; its cell size is
	// baked into every sixel payload.
	probe    *ttyapi.Probe
	mu       sync.Mutex
	opts     ttyapi.SurfaceOptions
	opened   bool
	acquired bool
	invalid  bool
	closed   bool
}

func NewSurface(out io.Writer, opts ttyapi.SurfaceOptions) *Surface {
	return NewProbedSurface(out, opts, ttyapi.ProcessProbe())
}

// NewProbedSurface presents to the terminal the probe describes. A remote
// session has its own: its protocol and cell size are not the server's.
func NewProbedSurface(out io.Writer, opts ttyapi.SurfaceOptions, probe *ttyapi.Probe) *Surface {
	if probe == nil {
		probe = ttyapi.ProcessProbe()
	}
	return &Surface{
		out: out, opts: opts, graphics: probedGraphics(probe), probe: probe,
		encodedLimit: defaultEncodedLimit,
	}
}

func (s *Surface) Present(frame ttyapi.Frame) (ttyapi.PresentStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ttyapi.PresentStats{}, fmt.Errorf("surface is closed")
	}
	s.frameSeq++
	output := s.scratch[:0]
	if s.opts.Synchronized {
		output = append(output, "\x1b[?2026h"...)
	}
	prefix := len(output)
	if !s.opened {
		if s.opts.AlternateScreen {
			output = append(output, "\x1b[?1049h"...)
		}
		if s.opts.HideCursor {
			output = append(output, "\x1b[?25l"...)
		}
	}
	// Sixel cannot take a picture off the screen by name; the only removal is
	// painting over the cells it held. Those rows have to be marked stale
	// BEFORE the comparison below, or a frame that drops a picture leaves it
	// on screen until something else happens to change the same line.
	s.forgetVacatedRows(frame.Placements)

	rows := frame.Rows
	changed, limit := 0, len(rows)
	if len(s.rows) > limit {
		limit = len(s.rows)
	}
	// Which rows were repainted. A placement sharing cells with one of them
	// lost that part of its picture, and nothing else would notice: for the
	// row itself the text is simply what it now says.
	var repainted []int
	for index := 0; index < limit; index++ {
		current, previous := "", ""
		if index < len(rows) {
			current = rows[index]
		}
		if index < len(s.rows) {
			previous = s.rows[index]
		}
		if !s.invalid && current == previous && index < len(rows) && index < len(s.rows) {
			continue
		}
		changed++
		if len(s.placements) > 0 {
			repainted = append(repainted, index+1)
		}
		output = append(output, '\x1b', '[')
		output = strconv.AppendInt(output, int64(index+1), 10)
		output = append(output, ';', '1', 'H')
		output = append(output, current...)
		output = append(output, "\x1b[0m\x1b[K"...)
	}

	output, placementsSent := s.appendPlacements(output, frame.Placements, repainted)
	if s.invalid && limit == 0 {
		output = append(output, "\x1b[H\x1b[0m\x1b[J"...)
	}
	// Painting rows moves the physical terminal cursor even when the logical
	// frame cursor itself did not change. Cursor placement is therefore dirty
	// whenever either cell damage or cursor state changed, and must be the last
	// operation in the frame transaction.
	effectiveCursor := frame.Cursor
	if effectiveCursor == nil {
		effectiveCursor = s.cursor
	}
	cursorChanged := frame.Cursor != nil && !sameSurfaceCursor(s.cursor, frame.Cursor)
	if effectiveCursor != nil && (s.invalid || changed != 0 || cursorChanged) {
		output = appendCursorTo(output, max(0, effectiveCursor.Row)+1, max(0, effectiveCursor.Column)+1)
		if effectiveCursor.Visible {
			output = append(output, "\x1b[?25h"...)
		} else {
			output = append(output, "\x1b[?25l"...)
		}
	}
	if len(output) == prefix {
		output = output[:0]
	} else if s.opts.Synchronized {
		output = append(output, "\x1b[?2026l"...)
	}
	if len(output) > 0 {
		// A short or failed write may still have changed terminal modes or cursor
		// state. Record acquisition before the call so Close performs recovery.
		s.acquired = true
		written, err := s.out.Write(output)
		if err != nil {
			return ttyapi.PresentStats{}, err
		}
		if written != len(output) {
			return ttyapi.PresentStats{}, io.ErrShortWrite
		}
	}
	s.opened = true
	s.invalid = false
	s.scratch = output
	s.rows = append(s.rows[:0], rows...)
	if frame.Cursor != nil {
		copy := *frame.Cursor
		s.cursor = &copy
	}
	return ttyapi.PresentStats{
		Rows:           len(rows),
		ChangedRows:    changed,
		Bytes:          len(output),
		PlacementsSent: placementsSent,
	}, nil
}

func sameSurfaceCursor(a, b *ttyapi.Cursor) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (s *Surface) Invalidate() {
	s.mu.Lock()
	if !s.closed {
		s.invalid = true
	}
	s.mu.Unlock()
}

func (s *Surface) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	if !s.acquired {
		return nil
	}
	restore := make([]byte, 0, 32)
	// Only kitty is told to forget its pictures. Sixel has no identifiers and
	// nothing to delete, and an APC command sent to a terminal that does not
	// speak the protocol is the very thing the frame path refuses to do — a
	// rule that holds in one of two places is a rule on its way out.
	if s.graphics == graphicsKitty {
		for id := range s.placements {
			restore = appendDelete(restore, id)
		}
	}
	s.placements = nil
	s.encoded, s.encodedBytes = nil, 0
	if s.opts.Synchronized {
		restore = append(restore, "\x1b[?2026l"...)
	}
	restore = append(restore, "\x1b[0m"...)
	restore = append(restore, "\x1b[?25h"...)
	if s.opts.AlternateScreen {
		restore = append(restore, "\x1b[?1049l"...)
	}
	written, err := s.out.Write(restore)
	if err != nil {
		s.closeErr = err
		return s.closeErr
	}
	if written != len(restore) {
		s.closeErr = io.ErrShortWrite
		return s.closeErr
	}
	return nil
}

var _ ttyapi.Surface = (*Surface)(nil)

func appendCursorTo(out []byte, row, column int) []byte {
	out = append(out, '\x1b', '[')
	out = strconv.AppendInt(out, int64(row), 10)
	out = append(out, ';')
	out = strconv.AppendInt(out, int64(column), 10)
	out = append(out, 'H')
	return out
}

// appendPlacements brings the screen's rasters in line with the frame.
//
// Placements are declarative and complete: one missing from the frame is
// taken off the screen. A caller that forgets a placement does not leak it,
// and a caller that repeats an unchanged one does not pay for it twice.
func (s *Surface) appendPlacements(out []byte, places []ttyapi.Placement, repainted []int) ([]byte, int) {
	sent := 0
	if s.graphics == graphicsNone {
		// The terminal shows no graphics. Sending anyway would spill the
		// escape sequence onto the screen as text, which is worse than the
		// missing picture: it looks like the application broke.
		return out, sent
	}
	if len(places) == 0 && len(s.placements) == 0 {
		return out, sent
	}
	if s.placements == nil {
		s.placements = make(map[string]placementState, len(places))
	}

	seen := make(map[string]bool, len(places))
	for _, place := range places {
		if place.ID == "" || place.Cols <= 0 || place.Rows <= 0 {
			continue
		}
		seen[place.ID] = true
		previous, known := s.placements[place.ID]

		damaged := s.invalid || !known || !previous.sameAs(place)
		if !damaged {
			for _, row := range repainted {
				if previous.covers(row) {
					damaged = true
					break
				}
			}
		}
		if !damaged {
			continue
		}
		if place.Image == nil {
			// Nothing to send and nothing on screen to keep: a placement
			// without pixels that was never transmitted cannot appear by
			// being asked for again.
			if !known {
				delete(seen, place.ID)
			}
			continue
		}
		if s.graphics == graphicsKitty && known && !s.invalid &&
			previous.serial == place.Serial && previous.version == place.Version {
			// The terminal still holds this image under the placement's id:
			// the row under it was repainted or it moved, and a put by id
			// costs a short command instead of the full RGBA. After
			// Invalidate the image is transmitted again — whatever reset the
			// screen may have taken the terminal's copy with it.
			out = appendPut(out, place)
		} else {
			out = s.appendEncoded(out, place)
		}
		s.placements[place.ID] = stateOf(place)
		sent++
	}

	for id := range s.placements {
		if !seen[id] {
			// Sixel has nothing to delete: the cells were already repainted
			// by forgetVacatedRows, and sending a kitty command to a terminal
			// that speaks sixel would spill onto the screen as text.
			if s.graphics == graphicsKitty {
				out = appendDelete(out, id)
			}
			delete(s.placements, id)
			s.dropEncoded(id)
		}
	}
	return out, sent
}

// appendEncoded writes the command for one placement, from the cache when the
// same key was encoded before.
func (s *Surface) appendEncoded(out []byte, place ttyapi.Placement) []byte {
	key := keyOf(s.graphics, place, s.probe)
	if entry, ok := s.encoded[place.ID]; ok && entry.key == key {
		entry.used = s.frameSeq
		s.encoded[place.ID] = entry
		return append(out, entry.bytes...)
	}
	start := len(out)
	if s.encode != nil {
		out = s.encode(out, s.graphics, place)
	} else {
		out = appendPlaceOn(out, s.graphics, place, s.probe)
	}
	s.dropEncoded(place.ID)
	// A failed encoding writes nothing, and nothing is not worth remembering:
	// the next send tries the encoder again.
	if s.encodedLimit <= 0 || len(out) == start {
		return out
	}
	if s.encoded == nil {
		s.encoded = make(map[string]encodedEntry)
	}
	payload := append([]byte(nil), out[start:]...)
	s.encoded[place.ID] = encodedEntry{bytes: payload, key: key, used: s.frameSeq}
	s.encodedBytes += len(payload)
	s.trimEncoded(place.ID)
	return out
}

// trimEncoded evicts the least recently written entries until the cache fits
// its limit. The entry just written goes last: evicting it first would make
// every frame miss.
func (s *Surface) trimEncoded(keep string) {
	for s.encodedBytes > s.encodedLimit {
		victim, oldest := "", uint64(0)
		for id, entry := range s.encoded {
			if id == keep {
				continue
			}
			if victim == "" || entry.used < oldest {
				victim, oldest = id, entry.used
			}
		}
		if victim == "" {
			s.dropEncoded(keep)
			return
		}
		s.dropEncoded(victim)
	}
}

func (s *Surface) dropEncoded(id string) {
	if entry, ok := s.encoded[id]; ok {
		s.encodedBytes -= len(entry.bytes)
		delete(s.encoded, id)
	}
}

// forgetVacatedRows marks the rows a picture is leaving as stale.
//
// Only sixel needs it — kitty removes by identifier — but the rule is the
// same either way: a rectangle a picture no longer occupies has to be painted
// again by whoever owns those cells, and the surface is the only thing that
// knows the picture was ever there.
func (s *Surface) forgetVacatedRows(places []ttyapi.Placement) {
	if s.graphics != graphicsSixel || len(s.placements) == 0 {
		return
	}
	kept := make(map[string]ttyapi.Placement, len(places))
	for _, place := range places {
		if place.ID != "" && place.Cols > 0 && place.Rows > 0 {
			kept[place.ID] = place
		}
	}
	for id, state := range s.placements {
		place, still := kept[id]
		// A picture that moved vacates its old rectangle just as surely as
		// one that disappeared.
		if still && state.row == place.Row && state.col == place.Col &&
			state.cols == place.Cols && state.rows == place.Rows {
			continue
		}
		for row := state.row; row < state.row+state.rows; row++ {
			if row >= 1 && row <= len(s.rows) {
				s.rows[row-1] = dirtyRow
			}
		}
	}
}
