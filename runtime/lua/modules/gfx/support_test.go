// SPDX-License-Identifier: MPL-2.0

package gfx

import (
	"testing"

	lua "github.com/wippyai/go-lua"
	ctxapi "github.com/wippyai/runtime/api/context"
	ttyapi "github.com/wippyai/runtime/api/tty"
)

type sessionPort struct{ probe *ttyapi.Probe }

func (p sessionPort) TerminalProbe() *ttyapi.Probe { return p.probe }

func askGfx(t *testing.T, l *lua.LState) (float64, float64, string) {
	t.Helper()
	mod, _ := buildModule()
	l.SetGlobal("gfx", mod)
	if err := l.DoString(`w, h = gfx.cell_size(); p = gfx.supported()`); err != nil {
		t.Fatal(err)
	}
	return float64(lua.LVAsNumber(l.GetGlobal("w"))), float64(lua.LVAsNumber(l.GetGlobal("h"))),
		lua.LVAsString(l.GetGlobal("p"))
}

// A remote session asks about its own screen, not the one the server was
// started on: two people rarely share a font.
func TestGfxAsksTheTerminalOfTheCallingProcess(t *testing.T) {
	ttyapi.SetProbedGraphics(ttyapi.GraphicsSixel, "")
	ttyapi.SetProbedCellSize(10, 20)
	t.Cleanup(ttyapi.ForgetProbedGraphics)
	probe := ttyapi.NewProbe(nil)
	probe.SetGraphics(ttyapi.GraphicsKitty, "")
	probe.SetCellSize(8, 16)

	ctx, fc := ctxapi.OpenFrameContext(ctxapi.NewRootContext())
	if err := fc.Set(ttyapi.PortKey(), sessionPort{probe: probe}); err != nil {
		t.Fatal(err)
	}
	session := lua.NewState()
	defer session.Close()
	session.SetContext(ctx)
	if w, h, p := askGfx(t, session); w != 8 || h != 16 || p != ttyapi.GraphicsKitty {
		t.Fatalf("session sees %vx%v %q, want 8x16 kitty", w, h, p)
	}

	server := lua.NewState()
	defer server.Close()
	if w, h, p := askGfx(t, server); w != 10 || h != 20 || p != ttyapi.GraphicsSixel {
		t.Fatalf("server's own terminal is %vx%v %q, want 10x20 sixel", w, h, p)
	}
}
