// SPDX-License-Identifier: MPL-2.0

package tty

import (
	"context"
	"os"
	"strings"
	"sync"

	ctxapi "github.com/wippyai/runtime/api/context"
)

// Graphics protocols a terminal may understand for showing rasters.
const (
	GraphicsNone  = ""
	GraphicsKitty = "kitty"
	GraphicsSixel = "sixel"
)

// Probe is what one terminal answered about itself, if anyone got round to
// asking.
//
// A process started from a shell has exactly one terminal behind it, and its
// answer is process-wide: that is the probe every package-level function
// below reads. A host serving remote terminals has one per connection, and
// two people rarely sit at the same screen — a cell size taken from the first
// would draw the second one's pictures subtly the wrong size. So the answer
// belongs to the terminal, and the process-wide one is merely the terminal
// the process was started on.
type Probe struct {
	env      func(string) string
	protocol string
	reason   string
	mu       sync.RWMutex
	answered bool
	silent   bool
	cellW    int
	cellH    int
}

// NewProbe returns an empty probe whose guesses read env. A remote terminal
// passes the variables its client sent, not the server's: those describe the
// machine the process runs on, which is the wrong side of the connection.
func NewProbe(env func(string) string) *Probe {
	if env == nil {
		env = func(string) string { return "" }
	}
	return &Probe{env: env}
}

// processProbe is the terminal this process was started on.
var processProbe = NewProbe(os.Getenv)

// ProcessProbe returns the probe of the terminal the process was started on.
func ProcessProbe() *Probe { return processProbe }

// ProbeSource is a port that knows which terminal it is attached to.
type ProbeSource interface {
	TerminalProbe() *Probe
}

// ProbeFromContext returns the probe of the terminal the calling process is
// attached to, and the process's own terminal otherwise.
//
// It looks at the port value without redeeming a lazy binding: asking about
// the screen must not take a viewport grant that belongs to someone else.
func ProbeFromContext(ctx context.Context) *Probe {
	if fc := ctxapi.FrameFromContext(ctx); fc != nil {
		if value, ok := fc.Get(portKey); ok {
			if source, ok := value.(ProbeSource); ok {
				if probe := source.TerminalProbe(); probe != nil {
					return probe
				}
			}
		}
	}
	return processProbe
}

// SetGraphics records what the terminal replied to a capability query.
//
// A refusal is worth recording too: "asked, and it said no" is a different
// fact from "nobody asked", and only the first one should stop the guessing
// below from having an opinion.
func (p *Probe) SetGraphics(protocol, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answered = true
	p.protocol = protocol
	p.reason = reason
}

// SetSilence records that the terminal was asked and said nothing at all.
//
// This does not decide anything — silence is not a refusal, and the guess
// below still gets its turn. It changes what the refusal says. A person told
// to go and look at KITTY_WINDOW_ID, when their terminal was asked directly
// and stayed quiet, is being sent to the wrong place: the variables were
// never the reason.
func (p *Probe) SetSilence() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.silent = true
}

// SetCellSize records how many pixels one character cell occupies.
//
// Sixel needs it for pixel positioning. Kitty can scale pictures to cells,
// but pixel UI must render at the actual cell size to avoid resampling text
// and icons.
//
// There is no default and no guess. A wrong cell size does not fail — it
// draws a picture that is subtly the wrong size, which reads as a bug in
// whatever composed it.
func (p *Probe) SetCellSize(width, height int) {
	if width <= 0 || height <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cellW, p.cellH = width, height
}

// CellSize reports the pixels in one cell, and whether anyone knows.
func (p *Probe) CellSize() (int, int, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cellW, p.cellH, p.cellW > 0 && p.cellH > 0
}

// Forget drops the recorded answer.
func (p *Probe) Forget() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answered = false
	p.protocol = ""
	p.reason = ""
	p.silent = false
	p.cellW, p.cellH = 0, 0
}

// Detect decides what this terminal can show, guessing from its own
// environment when it was not asked.
func (p *Probe) Detect() (string, string) {
	return p.detect(p.env)
}

func (p *Probe) wasSilent() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.silent
}

func (p *Probe) answer() (string, string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.protocol, p.reason, p.answered
}

// SetProbedGraphics records the answer of the terminal the process was
// started on.
//
// The host asks once, before any process runs, because the question travels
// out on the output path and the answer comes back on the input path — and
// the input path belongs to the running program a moment later.
func SetProbedGraphics(protocol, reason string) { processProbe.SetGraphics(protocol, reason) }

// SetGraphicsProbeSilence records that the process's terminal was asked and
// said nothing at all.
func SetGraphicsProbeSilence() { processProbe.SetSilence() }

// SetProbedCellSize records the cell size of the process's terminal. The host
// probes at startup; terminal resizes refresh it from the OS window
// dimensions when the terminal supplies pixel dimensions.
func SetProbedCellSize(width, height int) { processProbe.SetCellSize(width, height) }

// CellSize reports the pixels in one cell of the process's terminal.
func CellSize() (int, int, bool) { return processProbe.CellSize() }

// ForgetProbedGraphics drops the recorded answer. Tests use it; nothing else
// should, because the terminal does not change under a running process.
func ForgetProbedGraphics() { processProbe.Forget() }

// DetectGraphics decides what the process's terminal can show.
//
// It lives here, in the contract, because two things need the answer and they
// must not disagree: the surface, which decides whether to send anything, and
// the module that tells a program whether drawing is worth doing. Two copies
// of this rule already diverged once — the module said "no graphics" while
// the surface would happily have sent sixel.
//
// Three sources, in this order, and the order is the whole design:
//
//  1. The explicit switch. A person who names a protocol has looked at their
//     own screen, which beats any amount of inference; "off" has to win too,
//     or there is no way to turn a misbehaving terminal off.
//  2. What the terminal answered when asked. This is the only source that is
//     actually about the terminal in front of the person, rather than about
//     the machine the process happens to run on.
//  3. Environment variables. These describe the process's environment, and
//     over ssh that is the wrong side of the connection entirely — the
//     terminal's own variables stay with the client. Kept as a fallback for
//     the case where nobody asked, not as the answer.
//
// The second return value says what was looked at. A refusal that does not
// name what it wanted sends a person to read someone else's documentation at
// random.
func DetectGraphics(env func(string) string) (string, string) {
	return processProbe.detect(env)
}

func (p *Probe) detect(env func(string) string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(env("WIPPY_TTY_GRAPHICS"))) {
	case "off", "none":
		return GraphicsNone, "graphics switched off by WIPPY_TTY_GRAPHICS"
	case "kitty":
		return GraphicsKitty, ""
	case "sixel":
		return GraphicsSixel, ""
	case "":
	default:
		return GraphicsNone, "WIPPY_TTY_GRAPHICS must be kitty, sixel or off"
	}

	if protocol, reason, answered := p.answer(); answered {
		return protocol, reason
	}

	return p.guess(env)
}

// guess infers a protocol from the environment. It is a guess and is named
// one: everything it reads describes where the process runs, not what is
// drawing the screen.
func (p *Probe) guess(env func(string) string) (string, string) {
	if env("KITTY_WINDOW_ID") != "" {
		return GraphicsKitty, ""
	}
	if env("WEZTERM_PANE") != "" || env("WEZTERM_EXECUTABLE") != "" {
		return GraphicsKitty, ""
	}
	switch strings.ToLower(env("TERM_PROGRAM")) {
	case "wezterm", "ghostty":
		return GraphicsKitty, ""
	}
	term := strings.ToLower(env("TERM"))
	if strings.Contains(term, "kitty") || strings.Contains(term, "ghostty") {
		return GraphicsKitty, ""
	}
	if p.wasSilent() {
		return GraphicsNone, "the terminal was asked what it can draw and did not answer, " +
			"and nothing in the environment says either " +
			"(looked at KITTY_WINDOW_ID, WEZTERM_PANE, TERM_PROGRAM, TERM). " +
			"If it does show pictures, set WIPPY_TTY_GRAPHICS=sixel or =kitty"
	}
	return GraphicsNone, "terminal reports no graphics protocol " +
		"(looked at KITTY_WINDOW_ID, WEZTERM_PANE, TERM_PROGRAM, TERM). " +
		"Over ssh these stay on the other side: set WIPPY_TTY_GRAPHICS=sixel or =kitty"
}
