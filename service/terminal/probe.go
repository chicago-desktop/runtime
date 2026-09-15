// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/charmbracelet/x/term"
	ttyapi "github.com/wippyai/runtime/api/tty"
)

// Asking the terminal what it can draw, instead of guessing from the
// environment.
//
// The guess is wrong exactly where it matters most. Over ssh the variables a
// terminal sets about itself stay on the client's side, so a session that
// plainly does show pictures looks like one that cannot, and the person has
// to know to set a switch. Asking is the only way to learn about the screen
// in front of them rather than about the machine the process runs on.
//
// The question goes out on the output path and the answer comes back on the
// input path — the same path a running program owns a moment later. So this
// happens once, at host start, before any process exists, and never again.

// probeQuery asks two questions in one write.
//
// The kitty query first: a one-pixel image transmitted directly and never
// placed, which a terminal that speaks the protocol answers with OK and one
// that does not ignores, because APC is a sequence every terminal knows how
// to skip.
//
// Then Primary Device Attributes, which every terminal answers. That answer
// is what makes the wait bounded: it arrives after the kitty reply would
// have, so receiving it means the terminal has said everything it is going
// to say. Without a terminator the only alternative is a fixed sleep, which
// is either too short on a slow link or too long on every start.
// Three questions, then the terminator:
//
//	ESC _ G …            kitty: can you show a raster?
//	CSI 16 t             how many pixels is one cell?
//	CSI 14 t             how many pixels is the text area?
//	CSI c                Primary Device Attributes — the terminator
//
// Cell size determines sixel positioning and the native raster dimensions
// of pixel UI on both protocols. Rendering Kitty images at a stale size
// would make the terminal resample them to fit their cell rectangles.
//
// Both 16t and 14t are asked because terminals answer one or the other. 16t
// gives the number directly; 14t gives the text area, which divided by the
// screen size in cells gives the same answer and is the older, wider road.
const probeQuery = "\x1b_Gi=31,s=1,v=1,a=q,t=d,f=24;AAAA\x1b\\" +
	"\x1b[16t\x1b[14t\x1b[c"

// probeTimeout bounds the wait for a terminal that answers nothing at all.
// It is spent once per host start, and only by terminals that ignore Primary
// Device Attributes — which is close to none of them.
const probeTimeout = 300 * time.Millisecond

// probeReadSize is the read buffer. Replies are tens of bytes; the size is
// generous so a reply never arrives split across two polls for no reason.
const probeReadSize = 256

// errProbeUnsupported means the platform gives no way to wait for the
// terminal's answer without blocking a descriptor the program needs back.
var errProbeUnsupported = errors.New("terminal capability query not supported on this platform")

// probeTerminal asks the terminal and records the answer.
//
// It is deliberately quiet about failure. A terminal that cannot be asked is
// not broken — a pipe, a test harness, a platform without poll — and falling
// back to the environment is the behaviour that was there before. What must
// not happen is the probe reporting a refusal as if the terminal had said no:
// "nobody asked" and "it said no" are different facts, and only the second
// one should silence the guess.
func probeTerminal(stdin *os.File, out io.Writer, raw *RawManager) {
	if stdin == nil || out == nil || raw == nil {
		return
	}
	if err := raw.Enable(); err != nil {
		return
	}
	defer func() { _ = raw.Disable() }()

	if _, err := out.Write([]byte(probeQuery)); err != nil {
		return
	}

	reply, ok := readProbeReply(stdin, probeTimeout)
	if !ok {
		ttyapi.SetGraphicsProbeSilence()
		return
	}
	if width, height, ok := parseCellSize(reply); ok {
		ttyapi.SetProbedCellSize(width, height)
	} else if width, height, ok := parseTextArea(reply); ok {
		// The text area in pixels divided by the screen in cells. Done here
		// rather than by the caller because the division only makes sense
		// against the size the terminal had when it answered.
		if columns, rows, sized := terminalSize(stdin); sized && columns > 0 && rows > 0 {
			ttyapi.SetProbedCellSize(width/columns, height/rows)
		}
	}

	protocol, reason, answered := parseProbeReply(reply)
	if !answered {
		ttyapi.SetGraphicsProbeSilence()
		return
	}
	ttyapi.SetProbedGraphics(protocol, reason)
}

// readProbeReply collects bytes until the Primary Device Attributes reply
// closes the conversation, or until the deadline.
//
// Anything the person typed while this was in flight is read here and
// dropped: there is no way to put bytes back into a terminal, and the
// alternative — leaving a goroutine blocked in Read — loses a keystroke too,
// only later and with no idea which one. At host start, before any program is
// drawing, there is nothing to type at yet.
func readProbeReply(stdin *os.File, timeout time.Duration) ([]byte, bool) {
	deadline := time.Now().Add(timeout)
	var collected []byte
	buf := make([]byte, probeReadSize)

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return collected, len(collected) > 0
		}
		ready, err := waitReadable(int(stdin.Fd()), remaining)
		if err != nil || !ready {
			return collected, len(collected) > 0
		}
		n, err := stdin.Read(buf)
		if n > 0 {
			collected = append(collected, buf[:n]...)
			if _, closed := findDeviceAttributes(collected); closed {
				return collected, true
			}
		}
		if err != nil {
			return collected, len(collected) > 0
		}
	}
}

// parseProbeReply reads the terminal's answer.
//
// The third return value separates "the terminal answered" from "the
// terminal can draw". An answer naming no graphics protocol is still an
// answer, and it is the one that should stop the environment from guessing;
// a reply with no device attributes in it at all is not an answer, and the
// guess should still have its turn.
func parseProbeReply(reply []byte) (string, string, bool) {
	params, closed := findDeviceAttributes(reply)
	if !closed {
		return ttyapi.GraphicsNone, "", false
	}

	// Kitty first when both are on offer: it names its pictures, so one can
	// be replaced or taken off the screen. Sixel has no identifiers at all,
	// and removal there means painting over the cells.
	if kittyAcknowledged(reply) {
		return ttyapi.GraphicsKitty, "", true
	}
	for _, param := range params {
		if param == "4" {
			return ttyapi.GraphicsSixel, "", true
		}
	}
	return ttyapi.GraphicsNone,
		"terminal was asked and answered with no graphics protocol " +
			"(no kitty acknowledgement, no sixel in device attributes). " +
			"Override with WIPPY_TTY_GRAPHICS=sixel or =kitty", true
}

// kittyAcknowledged reports whether an APC in the reply is the protocol
// saying yes. The whole envelope is required: a bare "OK" somewhere in the
// stream is a keystroke, not an answer.
func kittyAcknowledged(reply []byte) bool {
	rest := reply
	for {
		start := bytes.Index(rest, []byte("\x1b_G"))
		if start < 0 {
			return false
		}
		rest = rest[start+3:]
		end := bytes.Index(rest, []byte("\x1b\\"))
		if end < 0 {
			return false
		}
		if bytes.Contains(rest[:end], []byte(";OK")) {
			return true
		}
		rest = rest[end+2:]
	}
}

// findDeviceAttributes returns the parameters of a Primary Device Attributes
// reply and whether a complete one was seen. Sixel support is parameter 4.
func findDeviceAttributes(reply []byte) ([]string, bool) {
	rest := reply
	for {
		start := bytes.Index(rest, []byte("\x1b[?"))
		if start < 0 {
			return nil, false
		}
		rest = rest[start+3:]
		end := bytes.IndexByte(rest, 'c')
		if end < 0 {
			return nil, false
		}
		body := rest[:end]
		// Another CSI starting inside means this one never terminated: the
		// 'c' belongs to something else further along.
		if bytes.IndexByte(body, '\x1b') >= 0 {
			continue
		}
		fields := bytes.Split(body, []byte(";"))
		params := make([]string, 0, len(fields))
		for _, field := range fields {
			params = append(params, string(bytes.TrimSpace(field)))
		}
		return params, true
	}
}

// terminalSize reports the screen in cells, for turning a text area in
// pixels into the size of one cell.
func terminalSize(stdin *os.File) (int, int, bool) {
	columns, rows, err := term.GetSize(stdin.Fd())
	if err != nil {
		return 0, 0, false
	}
	return columns, rows, true
}

// parseCellSize reads a reply to CSI 16 t: CSI 6 ; height ; width t.
func parseCellSize(reply []byte) (int, int, bool) {
	return parseWindowOp(reply, '6')
}

// parseTextArea reads a reply to CSI 14 t: CSI 4 ; height ; width t.
func parseTextArea(reply []byte) (int, int, bool) {
	return parseWindowOp(reply, '4')
}

// parseWindowOp finds a CSI <kind> ; height ; width t reply and returns
// width, height — in that order, because the wire order is the other way
// round and every caller here thinks in width-first.
//
// The leading kind matters: 4 and 6 arrive in the same stream from the same
// write, and taking whichever came first would report the text area as a cell
// on one terminal and the other way round on the next.
func parseWindowOp(reply []byte, kind byte) (int, int, bool) {
	rest := reply
	for {
		start := bytes.IndexByte(rest, '\x1b')
		if start < 0 || start+2 >= len(rest) {
			return 0, 0, false
		}
		rest = rest[start+1:]
		if rest[0] != '[' {
			continue
		}
		end := bytes.IndexByte(rest, 't')
		if end < 0 {
			return 0, 0, false
		}
		body := rest[1:end]
		if bytes.IndexByte(body, '\x1b') >= 0 {
			continue
		}
		fields := bytes.Split(body, []byte(";"))
		if len(fields) != 3 || len(fields[0]) != 1 || fields[0][0] != kind {
			rest = rest[end:]
			continue
		}
		height, errH := strconv.Atoi(string(fields[1]))
		width, errW := strconv.Atoi(string(fields[2]))
		if errH != nil || errW != nil || width <= 0 || height <= 0 {
			rest = rest[end:]
			continue
		}
		return width, height, true
	}
}
