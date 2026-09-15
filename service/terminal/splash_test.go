// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ttyapi "github.com/wippyai/runtime/api/tty"
)

func TestSplashFrameCentersWithoutStretchingOrFillingScreen(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 1448, 1086))
	for _, size := range [][4]int{{110, 34, 8, 18}, {80, 24, 10, 20}, {30, 12, 8, 16}, {200, 70, 10, 20}} {
		cols, rows, cw, ch := size[0], size[1], size[2], size[3]
		frame := splashFrame(source, cols, rows, cw, ch)
		p := frame.Placements[0]
		if p.Cols > cols-2 || p.Rows > rows-2 {
			t.Fatalf("oversized: %+v", p)
		}
		if p.Col != (cols-p.Cols)/2+1 || p.Row != (rows-p.Rows)/2+1 {
			t.Fatalf("not centered: %+v", p)
		}
		if p.Image.Bounds().Dx() > 640+cw || p.Image.Bounds().Dy() > 480+ch {
			t.Fatal("exceeds 640x480 size cap")
		}
		if len(frame.Rows) != rows || frame.Cursor.Visible {
			t.Fatal("invalid loading screen")
		}
	}
	large := splashFrame(source, 110, 34, 8, 18).Placements[0]
	if large.Cols != 80 || large.Rows != 27 {
		t.Fatal("a roomy terminal must display the image at 640x480, padded to cells")
	}
	small := image.NewRGBA(image.Rect(0, 0, 40, 30))
	p := splashFrame(small, 100, 40, 8, 16).Placements[0]
	if p.Cols != 5 || p.Rows != 2 {
		t.Fatal("must not enlarge a small image")
	}
	if len(splashFrame(source, 1, 1, 8, 16).Placements) != 0 {
		t.Fatal("tiny terminal must be left alone")
	}
}

func TestSplashSurfaceHandoffRemovesImageInFirstFrame(t *testing.T) {
	for _, protocol := range []graphicsProtocol{graphicsKitty, graphicsSixel} {
		t.Run(string(protocol), func(t *testing.T) {
			var out bytes.Buffer
			surface := NewSurface(&out, ttyapi.SurfaceOptions{AlternateScreen: true, HideCursor: true, Synchronized: true})
			surface.graphics = protocol
			img := image.NewRGBA(image.Rect(0, 0, 8, 8))
			img.Set(0, 0, color.White)
			first := splashFrame(img, 20, 10, 8, 16)
			if _, err := surface.Present(first); err != nil {
				t.Fatal(err)
			}
			splash := &Splash{surface: surface}
			ctx := WithSplash(context.Background(), splash)
			options := ttyapi.SurfaceOptions{AlternateScreen: true, HideCursor: true, Synchronized: true}
			app := SplashFromContext(ctx).TakeSurface(options)
			if app != surface || splash.TakeSurface(options) != nil {
				t.Fatal("surface must transfer exactly once")
			}
			out.Reset()
			if err := splash.Close(); err != nil || out.Len() != 0 {
				t.Fatal("bootstrap no longer owns surface")
			}
			if _, err := app.Present(ttyapi.Frame{Rows: []string{"desktop"}}); err != nil {
				t.Fatal(err)
			}
			emitted := out.String()
			if strings.Contains(emitted, "\x1b[?1049") {
				t.Fatal("handoff toggled alternate screen")
			}
			if len(app.placements) != 0 || !strings.Contains(emitted, "desktop") {
				t.Fatal("splash survived first app frame")
			}
			if !strings.HasPrefix(emitted, "\x1b[?2026h") || !strings.HasSuffix(emitted, "\x1b[?2026l") {
				t.Fatal("handoff must be one frame transaction")
			}
			if protocol == graphicsKitty && !strings.Contains(emitted, "a=d") {
				t.Fatal("kitty image not deleted")
			}
			if err := app.Close(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "\x1b[?1049l") {
				t.Fatal("application failed to restore original console")
			}
		})
	}
}

func TestSplashCloseRestoresTerminalBeforeApplicationOpens(t *testing.T) {
	var out bytes.Buffer
	surface := NewSurface(&out, ttyapi.SurfaceOptions{AlternateScreen: true, HideCursor: true})
	if _, err := surface.Present(ttyapi.Frame{Rows: []string{"loading"}}); err != nil {
		t.Fatal(err)
	}
	splash := &Splash{surface: surface}
	out.Reset()
	if err := splash.Close(); err != nil {
		t.Fatal(err)
	}
	restored := out.String()
	if !strings.Contains(restored, "\x1b[?1049l") || !strings.Contains(restored, "\x1b[?25h") {
		t.Fatal("startup failure leaves console acquired")
	}
	if err := splash.Close(); err != nil || out.String() != restored {
		t.Fatal("close must be idempotent")
	}
	if splash.TakeSurface(ttyapi.SurfaceOptions{AlternateScreen: true}) != nil {
		t.Fatal("closed splash cannot transfer")
	}
}

func TestReadSplashRejectsBrokenFileAndLoadsPNG(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logo.png")
	if _, err := readSplash(path); err == nil {
		t.Fatal("missing image must be named")
	}
	if err := os.WriteFile(path, []byte("not an image"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSplash(path); err == nil {
		t.Fatal("broken image must be named")
	}
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 20, 10))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	img, err := readSplash(path)
	if err != nil || img.Bounds().Dx() != 20 {
		t.Fatalf("valid image failed: %v", err)
	}
}
