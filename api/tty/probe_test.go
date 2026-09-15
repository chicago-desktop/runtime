// SPDX-License-Identifier: MPL-2.0

package tty

import (
	"testing"

	ctxapi "github.com/wippyai/runtime/api/context"
)

type probePort struct{ probe *Probe }

func (p probePort) TerminalProbe() *Probe { return p.probe }

func TestProbesAreSeparateTerminals(t *testing.T) {
	t.Cleanup(ForgetProbedGraphics)
	remote := NewProbe(func(name string) string {
		if name == "TERM" {
			return "xterm-kitty"
		}
		return ""
	})
	remote.SetCellSize(8, 16)
	remote.SetGraphics(GraphicsSixel, "")
	SetProbedCellSize(10, 20)

	if w, h, known := remote.CellSize(); !known || w != 8 || h != 16 {
		t.Fatalf("remote cell = %dx%d (%v), want 8x16", w, h, known)
	}
	if w, h, known := CellSize(); !known || w != 10 || h != 20 {
		t.Fatalf("process cell = %dx%d (%v), want 10x20: a session's answer leaked into the process's", w, h, known)
	}
	if protocol, _ := remote.Detect(); protocol != GraphicsSixel {
		t.Fatalf("answered protocol = %q, want sixel over the TERM guess", protocol)
	}
}

func TestProbeGuessesFromItsOwnEnvironment(t *testing.T) {
	remote := NewProbe(func(name string) string {
		if name == "TERM" {
			return "xterm-kitty"
		}
		return ""
	})
	if protocol, _ := remote.Detect(); protocol != GraphicsKitty {
		t.Fatalf("guess = %q, want kitty from the client's TERM", protocol)
	}
	silent := NewProbe(nil)
	silent.SetSilence()
	if protocol, reason := silent.Detect(); protocol != GraphicsNone || reason == "" {
		t.Fatalf("silent probe = %q %q, want none with a reason", protocol, reason)
	}
}

func TestProbeFromContext(t *testing.T) {
	if got := ProbeFromContext(t.Context()); got != ProcessProbe() {
		t.Fatal("a context without a frame must answer with the process's terminal")
	}

	remote := NewProbe(nil)
	ctx, fc := ctxapi.OpenFrameContext(ctxapi.NewRootContext())
	if err := fc.Set(PortKey(), probePort{probe: remote}); err != nil {
		t.Fatal(err)
	}
	if got := ProbeFromContext(ctx); got != remote {
		t.Fatal("a port that knows its terminal must be asked, not the process")
	}

	ctx2, fc2 := ctxapi.OpenFrameContext(ctxapi.NewRootContext())
	if err := fc2.Set(PortKey(), probePort{}); err != nil {
		t.Fatal(err)
	}
	if got := ProbeFromContext(ctx2); got != ProcessProbe() {
		t.Fatal("a port without a probe of its own is the process's terminal")
	}
}
