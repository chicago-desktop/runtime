// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"testing"

	ttyapi "github.com/wippyai/runtime/api/tty"
)

func TestParseProbeReplyReadsWhatTerminalsActuallySend(t *testing.T) {
	tests := []struct {
		name     string
		reply    string
		protocol string
		answered bool
	}{
		{
			name:     "kitty acknowledges its own query",
			reply:    "\x1b_Gi=31;OK\x1b\\\x1b[?62;1;6;22c",
			protocol: ttyapi.GraphicsKitty,
			answered: true,
		},
		{
			name:     "device attributes naming 4 mean sixel",
			reply:    "\x1b[?62;1;4;6;22c",
			protocol: ttyapi.GraphicsSixel,
			answered: true,
		},
		{
			name:     "kitty wins when the terminal offers both",
			reply:    "\x1b_Gi=31;OK\x1b\\\x1b[?62;4;22c",
			protocol: ttyapi.GraphicsKitty,
			answered: true,
		},
		{
			name:     "an answer naming nothing is still an answer",
			reply:    "\x1b[?62;1;6;22c",
			protocol: ttyapi.GraphicsNone,
			answered: true,
		},
		{
			// 14 contains a 4 and 41 starts with one. A substring search over
			// the parameter list calls a terminal without sixel a terminal
			// with it, and nothing downstream would ever notice.
			name:     "a 4 inside another parameter is not sixel",
			reply:    "\x1b[?64;14;41;22c",
			protocol: ttyapi.GraphicsNone,
			answered: true,
		},
		{
			name:     "no device attributes means the terminal never answered",
			reply:    "\x1b_Gi=31;OK\x1b\\",
			protocol: ttyapi.GraphicsNone,
			answered: false,
		},
		{
			name:     "silence is not an answer",
			reply:    "",
			protocol: ttyapi.GraphicsNone,
			answered: false,
		},
		{
			// The person leaning on a key while the host starts. Those bytes
			// are read by the probe and must not be mistaken for a reply.
			name:     "keystrokes alone are not an answer",
			reply:    "OK4cq",
			protocol: ttyapi.GraphicsNone,
			answered: false,
		},
		{
			name:     "keystrokes around a real answer do not disturb it",
			reply:    "j\x1b[?62;1;4;22ck",
			protocol: ttyapi.GraphicsSixel,
			answered: true,
		},
		{
			name:     "an APC without OK is a refusal, not an acknowledgement",
			reply:    "\x1b_Gi=31;ENOTSUPPORTED:no graphics\x1b\\\x1b[?62;22c",
			protocol: ttyapi.GraphicsNone,
			answered: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			protocol, reason, answered := parseProbeReply([]byte(test.reply))
			if answered != test.answered {
				t.Fatalf("answered: got %v, want %v", answered, test.answered)
			}
			if protocol != test.protocol {
				t.Fatalf("protocol: got %q, want %q", protocol, test.protocol)
			}
			if answered && protocol == ttyapi.GraphicsNone && reason == "" {
				t.Fatal("a refusal must say what it looked at")
			}
		})
	}
}

func TestFindDeviceAttributesWaitsForTheTerminator(t *testing.T) {
	// The reply arrives in pieces over a slow link. A parser that accepts a
	// half-read one stops listening early and reports whatever it has.
	if _, closed := findDeviceAttributes([]byte("\x1b[?62;1;4")); closed {
		t.Fatal("an unterminated reply must not count as closed")
	}
	if _, closed := findDeviceAttributes([]byte("\x1b[?62;1;4;22c")); !closed {
		t.Fatal("a terminated reply must count as closed")
	}
}

func TestProbedAnswerBeatsTheEnvironmentGuess(t *testing.T) {
	// The case this whole thing exists for: over ssh the environment knows
	// nothing, and the terminal knows everything.
	ttyapi.ForgetProbedGraphics()
	defer ttyapi.ForgetProbedGraphics()

	empty := func(string) string { return "" }
	if protocol, _ := ttyapi.DetectGraphics(empty); protocol != ttyapi.GraphicsNone {
		t.Fatalf("without an answer the guess should find nothing, got %q", protocol)
	}

	ttyapi.SetProbedGraphics(ttyapi.GraphicsSixel, "")
	if protocol, _ := ttyapi.DetectGraphics(empty); protocol != ttyapi.GraphicsSixel {
		t.Fatalf("the terminal's own answer should win, got %q", protocol)
	}
}

func TestExplicitSwitchBeatsTheProbe(t *testing.T) {
	// Someone looking at their own screen outranks the terminal's opinion of
	// itself, and "off" has to be reachable for a terminal that answers yes
	// and then draws rubbish.
	ttyapi.ForgetProbedGraphics()
	defer ttyapi.ForgetProbedGraphics()
	ttyapi.SetProbedGraphics(ttyapi.GraphicsKitty, "")

	env := func(key string) string {
		if key == "WIPPY_TTY_GRAPHICS" {
			return "off"
		}
		return ""
	}
	protocol, reason := ttyapi.DetectGraphics(env)
	if protocol != ttyapi.GraphicsNone {
		t.Fatalf("the switch should win over the probe, got %q", protocol)
	}
	if reason == "" {
		t.Fatal("switching graphics off should say so")
	}
}

func TestProbedRefusalSilencesTheGuess(t *testing.T) {
	// A terminal that was asked and said no must not be second-guessed by a
	// variable describing the machine it happens to run on.
	ttyapi.ForgetProbedGraphics()
	defer ttyapi.ForgetProbedGraphics()
	ttyapi.SetProbedGraphics(ttyapi.GraphicsNone, "asked and answered no")

	env := func(key string) string {
		if key == "KITTY_WINDOW_ID" {
			return "1"
		}
		return ""
	}
	if protocol, _ := ttyapi.DetectGraphics(env); protocol != ttyapi.GraphicsNone {
		t.Fatalf("a recorded refusal should stand, got %q", protocol)
	}
}

func TestSilenceChangesWhatTheRefusalSays(t *testing.T) {
	// Silence is not a refusal — the guess still gets its turn — but a person
	// whose terminal was asked and stayed quiet must not be sent to check
	// environment variables that were never the reason.
	ttyapi.ForgetProbedGraphics()
	defer ttyapi.ForgetProbedGraphics()

	empty := func(string) string { return "" }
	_, before := ttyapi.DetectGraphics(empty)

	ttyapi.SetGraphicsProbeSilence()
	protocol, after := ttyapi.DetectGraphics(empty)

	if protocol != ttyapi.GraphicsNone {
		t.Fatalf("silence must not invent a protocol, got %q", protocol)
	}
	if after == before {
		t.Fatal("a terminal that was asked and said nothing should say so")
	}

	// And it must not shout down a terminal the environment does know.
	kitty := func(key string) string {
		if key == "KITTY_WINDOW_ID" {
			return "1"
		}
		return ""
	}
	if protocol, _ := ttyapi.DetectGraphics(kitty); protocol != ttyapi.GraphicsKitty {
		t.Fatalf("silence must leave the guess its turn, got %q", protocol)
	}
}

func TestCellSizeIsReadFromTheRightReply(t *testing.T) {
	// 4 and 6 arrive in the same stream from the same write. Taking whichever
	// came first reports the text area as a cell on one terminal and the
	// other way round on the next — and both look plausible.
	reply := []byte("\x1b[6;16;8t\x1b[4;448;800t\x1b[?62;1;4;22c")

	width, height, ok := parseCellSize(reply)
	if !ok || width != 8 || height != 16 {
		t.Fatalf("cell size: got %dx%d ok=%v, want 8x16", width, height, ok)
	}
	width, height, ok = parseTextArea(reply)
	if !ok || width != 800 || height != 448 {
		t.Fatalf("text area: got %dx%d ok=%v, want 800x448", width, height, ok)
	}
}

func TestCellSizeInEitherOrder(t *testing.T) {
	// Terminals answer in whatever order they like, and some answer only one.
	only14 := []byte("\x1b[4;448;800t\x1b[?62;4;22c")
	if _, _, ok := parseCellSize(only14); ok {
		t.Fatal("a text-area reply must not be read as a cell size")
	}
	if width, height, ok := parseTextArea(only14); !ok || width != 800 || height != 448 {
		t.Fatalf("text area alone: got %dx%d ok=%v", width, height, ok)
	}

	swapped := []byte("\x1b[4;448;800t\x1b[6;16;8t\x1b[?62;4;22c")
	if width, height, ok := parseCellSize(swapped); !ok || width != 8 || height != 16 {
		t.Fatalf("order must not matter: got %dx%d ok=%v", width, height, ok)
	}
}

func TestNoCellSizeIsSaidPlainly(t *testing.T) {
	// A terminal that says nothing about cells must leave the answer unknown.
	// Eight by sixteen is right often enough to look correct and wrong often
	// enough to be blamed on whatever drew the picture.
	if _, _, ok := parseCellSize([]byte("\x1b[?62;4;22c")); ok {
		t.Fatal("silence must not produce a cell size")
	}
	if _, _, ok := parseWindowOp([]byte("\x1b[6;0;0t"), '6'); ok {
		t.Fatal("a zero-sized cell is not an answer")
	}
}
