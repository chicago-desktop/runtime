// SPDX-License-Identifier: MPL-2.0

package tty

import (
	"context"
	"image"
	"io"

	ctxapi "github.com/wippyai/runtime/api/context"
)

// SurfaceOptions describe presentation behavior. Physical ports may translate
// these to terminal modes; virtual ports retain them as surface metadata.
type SurfaceOptions struct {
	AlternateScreen bool
	HideCursor      bool
	Synchronized    bool
}

type PresentStats struct {
	Rows        int
	ChangedRows int
	Bytes       int

	// Rasters actually sent this frame, out of however many the frame
	// declared. It is the number a caller drawing an interface in pixels has
	// to watch: a placement is transmitted whenever its pixels change OR a
	// text row under it is repainted, so a chrome cut into the wrong pieces
	// resends everything on every keystroke — silently, and only visible as
	// the whole thing feeling slow.
	PlacementsSent int
}

// Cursor is terminal cursor state in zero-based surface coordinates.
type Cursor struct {
	Column  int
	Row     int
	Visible bool
}

// Placement is a raster occupying a rectangle of cells. Rows describe text;
// a placement describes pixels that survive between frames on their own, so
// the surface has to remember what it put on screen and take it away itself.
//
// Version changes when the pixels change. The surface retransmits only when
// it does — a raster resent every frame is the difference between a still
// picture and a flickering one, and the sender is the only one who can tell
// cheaply whether anything moved.
type Placement struct {
	Image   image.Image
	ID      string
	Version uint64

	// Identifies the buffer, as opposed to its contents. Two different
	// pictures can carry the same version — a caller that rebuilds a raster
	// every frame restarts the count — and without this the surface would
	// take the second for the first and leave the stale one on screen.
	Serial uint64
	Row    int
	Col    int
	Cols   int
	Rows   int
}

// Frame augments surface rows with optional terminal state. A nil Cursor lets
// a renderer preserve its configured cursor behavior.
//
// Placements are declarative and complete: a placement missing from a frame
// is removed from the screen. A caller that forgets one does not leak it.
type Frame struct {
	Cursor     *Cursor
	Rows       []string
	Placements []Placement
}

type RawController interface {
	Enable() error
	Disable() error
	Reset() error
	Enabled() bool
}

// Surface is the presentation side of a terminal port.
type Surface interface {
	// Present atomically publishes cells and optional cursor state. A nil cursor
	// preserves the last explicit cursor state.
	Present(Frame) (PresentStats, error)
	// Invalidate forgets backend presentation state without changing the last
	// published frame. The next Present must commit even if its content is
	// otherwise unchanged.
	Invalidate()
	// Close releases presentation ownership and must be safe to call more than
	// once. Callers receive the result of the first close attempt.
	Close() error
}

// Port is the minimal process-scoped terminal attachment. Implementations must
// be safe to close more than once. Byte streams and raw mode are optional and
// deliberately live on StreamPort rather than being faked by virtual ports.
type Port interface {
	InputController() InputController
	// OpenSurface acquires the port's exclusive presentation lease. A port has
	// one producer, so concurrent surfaces are rejected until the open surface
	// is closed.
	OpenSurface(SurfaceOptions) (Surface, error)
	Close() error
}

// StreamPort augments a Port with physical byte streams and raw-mode control.
// Host terminals implement it; structured virtual viewports need not.
type StreamPort interface {
	Port
	RawController() RawController
	Reader() io.Reader
	Output() io.Writer
	ErrorOutput() io.Writer
}

// Binding delays grant redemption until the destination process frame exists.
type Binding interface {
	Resolve(context.Context) (Port, error)
	Close() error
}

var portKey = &ctxapi.Key{Name: "tty.port"} // deliberately non-inheritable

func PortKey() *ctxapi.Key { return portKey }

func PortPair(port Port) ctxapi.Pair { return ctxapi.Pair{Key: portKey, Value: port} }

func BindingPair(binding Binding) ctxapi.Pair {
	return ctxapi.Pair{Key: portKey, Value: binding}
}

func WithPort(ctx context.Context, port Port) error {
	fc := ctxapi.FrameFromContext(ctx)
	if fc == nil {
		return ctxapi.ErrNoFrameContext
	}
	return fc.Set(portKey, port)
}

// GetPort returns the frame-owned port, redeeming a lazy binding once.
func GetPort(ctx context.Context) (Port, error) {
	fc := ctxapi.FrameFromContext(ctx)
	if fc == nil {
		return nil, nil
	}
	v, ok := fc.Get(portKey)
	if !ok {
		return nil, nil
	}
	if port, ok := v.(Port); ok {
		return port, nil
	}
	if binding, ok := v.(Binding); ok {
		return binding.Resolve(ctx)
	}
	return nil, ErrInvalidPort
}
