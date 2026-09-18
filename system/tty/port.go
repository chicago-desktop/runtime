// SPDX-License-Identifier: MPL-2.0

package tty

import (
	"sync"
	"sync/atomic"

	"github.com/wippyai/runtime/api/pid"
	ttyapi "github.com/wippyai/runtime/api/tty"
)

type port struct {
	session   *session
	input     *input
	surface   *surface
	once      sync.Once
	surfaceMu sync.Mutex
	closed    bool
}

func (p *port) InputController() ttyapi.InputController { return p.input }
func (p *port) OpenSurface(ttyapi.SurfaceOptions) (ttyapi.Surface, error) {
	p.surfaceMu.Lock()
	defer p.surfaceMu.Unlock()
	if p.closed {
		return nil, ttyapi.ErrInvalidPort
	}
	if p.surface != nil {
		return nil, ttyapi.ErrSurfaceOpen
	}
	s := &surface{session: p.session, owner: p}
	p.surface = s
	return s, nil
}
func (p *port) Close() error {
	p.once.Do(func() {
		p.surfaceMu.Lock()
		p.closed = true
		surface := p.surface
		p.surfaceMu.Unlock()
		if surface != nil {
			_ = surface.Close()
		}
		ss := p.session
		ss.mu.Lock()
		ss.inputOpen, ss.producer = false, false
		ss.target, ss.router = pid.PID{}, nil
		ss.mu.Unlock()
		ss.service.collect(ss)
	})
	return nil
}

type surface struct {
	session *session
	owner   *port
	once    sync.Once
	closed  atomic.Bool
}

func (s *surface) Present(frame ttyapi.Frame) (ttyapi.PresentStats, error) {
	ss := s.session
	ss.mu.Lock()
	if s.closed.Load() || ss.closed || !ss.producer {
		ss.mu.Unlock()
		return ttyapi.PresentStats{}, ttyapi.ErrViewportClosed
	}
	changed := changedRows(ss.rows, frame.Rows)
	forced := ss.invalid
	if forced && changed == 0 {
		changed = len(frame.Rows)
	}
	// A nil cursor means row-only presentation and preserves terminal state,
	// matching the Frame contract and physical surface implementation.
	cursorChanged := frame.Cursor != nil && !sameCursor(ss.cursor, frame.Cursor)
	placements, placementsChanged := mergePlacements(ss.placements, frame.Placements)
	if forced || changed != 0 || cursorChanged || placementsChanged {
		ss.rows = append([]string(nil), frame.Rows...)
		ss.placements = placements
		ss.invalid = false
		if frame.Cursor != nil {
			copy := *frame.Cursor
			ss.cursor = &copy
		}
		ss.revision++
		update := ttyapi.Update{Revision: ss.revision}
		for _, watcher := range ss.watches {
			publishLatest(watcher.ch, update)
		}
	}
	ss.mu.Unlock()
	return ttyapi.PresentStats{Rows: len(frame.Rows), ChangedRows: changed,
		PlacementsSent: len(placements)}, nil
}

// mergePlacements turns the frame's placements into the complete set a viewer
// can draw from on its own, and reports whether that set differs from the one
// standing on the screen.
//
// A frame is declarative and complete, so the result is built from it alone —
// what it leaves out is gone. The only thing carried over is pixels: a
// placement presented without an image means "the picture under this id has
// not changed", and a viewer that attached after the frame that carried it
// would otherwise have nothing to draw.
//
// A placement with no image whose id was never seen is left out. Its pixels
// have never existed anywhere, so no viewer could draw it; keeping it would
// put a picture on the screen that is not a picture.
//
// Sameness is id, version, serial, geometry and stacking — the identity the
// physical surface already uses to decide whether pixels must be resent. The
// images themselves are never compared: Version and Serial exist precisely so
// that nobody has to, and comparing two image.Image values can panic on a
// type that is not comparable.
func mergePlacements(prev, next []ttyapi.Placement) ([]ttyapi.Placement, bool) {
	if len(prev) == 0 && len(next) == 0 {
		return nil, false
	}
	out := make([]ttyapi.Placement, 0, len(next))
	for _, placement := range next {
		if placement.Image == nil {
			known, ok := findPlacement(prev, placement.ID)
			if !ok || known.Image == nil {
				continue
			}
			placement.Image = known.Image
		}
		out = append(out, placement)
	}
	if len(out) == 0 {
		out = nil
	}
	return out, !samePlacements(prev, out)
}

func findPlacement(in []ttyapi.Placement, id string) (ttyapi.Placement, bool) {
	for _, placement := range in {
		if placement.ID == id {
			return placement, true
		}
	}
	return ttyapi.Placement{}, false
}

func samePlacements(a, b []ttyapi.Placement) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Version != b[i].Version || a[i].Serial != b[i].Serial ||
			a[i].Row != b[i].Row || a[i].Col != b[i].Col ||
			a[i].Cols != b[i].Cols || a[i].Rows != b[i].Rows || a[i].Z != b[i].Z {
			return false
		}
	}
	return true
}

// publishLatest replaces the single buffered watermark. Callers serialize
// producers with session.mu, so no update can race the replacement.
func publishLatest(ch chan ttyapi.Update, update ttyapi.Update) {
	select {
	case ch <- update:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- update:
	default:
	}
}

func sameCursor(a, b *ttyapi.Cursor) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (s *surface) Invalidate() {
	s.owner.surfaceMu.Lock()
	active := s.owner.surface == s && !s.closed.Load()
	if !active {
		s.owner.surfaceMu.Unlock()
		return
	}
	s.session.mu.Lock()
	s.session.invalid = true
	s.session.mu.Unlock()
	s.owner.surfaceMu.Unlock()
}

func (s *surface) Close() error {
	s.once.Do(func() {
		s.closed.Store(true)
		s.owner.surfaceMu.Lock()
		if s.owner.surface == s {
			s.owner.surface = nil
		}
		s.owner.surfaceMu.Unlock()
	})
	return nil
}

func changedRows(a, b []string) int {
	limit, changed := len(a), 0
	if len(b) > limit {
		limit = len(b)
	}
	for i := 0; i < limit; i++ {
		if i >= len(a) || i >= len(b) || a[i] != b[i] {
			changed++
		}
	}
	return changed
}

var _ ttyapi.Surface = (*surface)(nil)

type input struct {
	session *session
}

func (i *input) Start() error {
	i.session.mu.Lock()
	defer i.session.mu.Unlock()
	if i.session.closed || !i.session.producer {
		return ttyapi.ErrViewportClosed
	}
	i.session.inputOpen = true
	return nil
}
func (i *input) Stop() error {
	i.session.mu.Lock()
	i.session.inputOpen = false
	i.session.mu.Unlock()
	return nil
}
func (i *input) ScreenSize() (int, int, error) {
	i.session.mu.RLock()
	defer i.session.mu.RUnlock()
	return i.session.width, i.session.height, nil
}
func (i *input) EnableMouse()  {}
func (i *input) DisableMouse() {}

// TerminalProbe makes the port a ttyapi.ProbeSource, so a producer inside a
// viewport asking what terminal it is drawing on is answered about the
// VIEWER's terminal rather than the process's own.
//
// Without this the answer came from the server's environment, which on
// another node is not even the same machine: a nested desktop would ask
// whether it may draw pictures, hear nothing, and fall back to cells for
// good. The probe stays unanswered until a viewer fills it in, so cells
// remain the default — a desktop drawn in cells is plain, one that spills
// raster escapes onto a terminal that cannot show them is broken.
func (p *port) TerminalProbe() *ttyapi.Probe { return p.session.probe }

var _ ttyapi.ProbeSource = (*port)(nil)
