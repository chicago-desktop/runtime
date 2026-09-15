// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	"os"
	"strings"
	"sync"

	// Register the GIF, JPEG and PNG decoders with image.Decode, which the
	// splash uses to read the picture file it shows.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/charmbracelet/x/term"
	ttyapi "github.com/wippyai/runtime/api/tty"
	"golang.org/x/image/draw"
)

// Splash owns a temporary physical surface until the application opens its
// fullscreen surface. Reusing the surface makes the first app frame remove the
// image in the same transaction, including on terminals using sixel.
type Splash struct {
	surface *Surface
	mu      sync.Mutex
}

type splashKey struct{}

func WithSplash(ctx context.Context, splash *Splash) context.Context {
	if splash == nil {
		return ctx
	}
	return context.WithValue(ctx, splashKey{}, splash)
}

func SplashFromContext(ctx context.Context) *Splash {
	if ctx == nil {
		return nil
	}
	splash, _ := ctx.Value(splashKey{}).(*Splash)
	return splash
}

// StartSplash loads a local image before modules and their filesystems exist.
// Pipes and terminals without graphics or known cell geometry are left alone.
// PNG, JPEG and the first frame of a GIF are supported.
func StartSplash(path string) (*Splash, error) {
	if path == "" || !term.IsTerminal(os.Stdout.Fd()) || !term.IsTerminal(os.Stdin.Fd()) {
		return nil, nil
	}
	img, err := readSplash(path)
	if err != nil {
		return nil, err
	}
	probeOnce.Do(func() { probeTerminal(os.Stdin, os.Stdout, NewRawManager(os.Stdin)) })
	cw, ch, sized := ttyapi.CellSize()
	cols, rows, screenSized := terminalSize(os.Stdin)
	if !sized || !screenSized || environmentGraphics() == graphicsNone {
		return nil, nil
	}
	frame := splashFrame(img, cols, rows, cw, ch)
	if len(frame.Placements) == 0 {
		return nil, nil
	}
	surface := NewSurface(os.Stdout, ttyapi.SurfaceOptions{AlternateScreen: true, HideCursor: true, Synchronized: true})
	if _, err := surface.Present(frame); err != nil {
		_ = surface.Close()
		return nil, err
	}
	return &Splash{surface: surface}, nil
}

func readSplash(path string) (image.Image, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open startup image: %w", err)
	}
	defer file.Close()
	const maxBytes = 16 << 20
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, fmt.Errorf("startup image must be a regular file of at most 16 MiB")
	}
	cfg, _, err := image.DecodeConfig(io.LimitReader(file, maxBytes))
	if err != nil {
		return nil, fmt.Errorf("read startup image: %w", err)
	}
	if cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > 16_000_000 {
		return nil, fmt.Errorf("startup image exceeds 16 megapixels")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(io.LimitReader(file, maxBytes))
	if err != nil {
		return nil, fmt.Errorf("decode startup image: %w", err)
	}
	return img, nil
}

// splashFrame preserves the source aspect ratio inside 640x480 pixels with a
// one-cell margin around the terminal. Padding aligns it with the cell
// grid without stretching. It never enlarges a smaller source image.
func splashFrame(img image.Image, cols, rows, cw, ch int) ttyapi.Frame {
	if img == nil || cols < 3 || rows < 3 || cw < 1 || ch < 1 {
		return ttyapi.Frame{}
	}
	bounds := img.Bounds()
	if bounds.Empty() {
		return ttyapi.Frame{}
	}
	maxW, maxH := min(640, (cols-2)*cw), min(480, (rows-2)*ch)
	scale := min(1.0, float64(maxW)/float64(bounds.Dx()), float64(maxH)/float64(bounds.Dy()))
	w, h := max(1, int(math.Floor(float64(bounds.Dx())*scale))), max(1, int(math.Floor(float64(bounds.Dy())*scale)))
	pc, pr := (w+cw-1)/cw, (h+ch-1)/ch
	raster := image.NewRGBA(image.Rect(0, 0, pc*cw, pr*ch))
	draw.Draw(raster, raster.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
	x, y := (pc*cw-w)/2, (pr*ch-h)/2
	draw.ApproxBiLinear.Scale(raster, image.Rect(x, y, x+w, y+h), img, bounds, draw.Over, nil)
	frame := ttyapi.Frame{Rows: make([]string, rows), Cursor: &ttyapi.Cursor{Visible: false}}
	blank := "\x1b[48;2;0;0;0m" + strings.Repeat(" ", cols) + "\x1b[0m"
	for i := range frame.Rows {
		frame.Rows[i] = blank
	}
	frame.Placements = []ttyapi.Placement{{ID: "wippy:startup", Serial: 1, Version: 1,
		Image: raster, Col: (cols-pc)/2 + 1, Row: (rows-pr)/2 + 1, Cols: pc, Rows: pr}}
	return frame
}

// TakeSurface transfers ownership once. An ordinary inline surface cannot
// inherit the alternate screen, so that path dismisses the splash first.
func (s *Splash) TakeSurface(options ttyapi.SurfaceOptions) *Surface {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	surface := s.surface
	if surface == nil {
		return nil
	}
	s.surface = nil
	if !options.AlternateScreen {
		_ = surface.Close()
		return nil
	}
	surface.mu.Lock()
	surface.opts = options
	surface.invalid = true
	if !options.HideCursor {
		surface.cursor = &ttyapi.Cursor{Visible: true}
	}
	surface.mu.Unlock()
	return surface
}

// Close restores the console on startup failure or cancellation. After a
// transfer, the application's surface owns that cleanup and this is a no-op.
func (s *Splash) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.surface == nil {
		return nil
	}
	surface := s.surface
	s.surface = nil
	return surface.Close()
}
