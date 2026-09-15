// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/registry"
	terminalapi "github.com/wippyai/runtime/api/service/terminal"
	"github.com/wippyai/runtime/system/logs"
	"github.com/wippyai/runtime/system/scheduler/actor"
	"go.uber.org/zap"
)

func newModesHost(t *testing.T, out *bytes.Buffer) *Host {
	t.Helper()
	scheduler := actor.NewScheduler(&mockCommandRegistry{}, actor.WithWorkers(1))
	h := NewHost(registry.ID{NS: "test", Name: "modes"}, &terminalapi.HostConfig{},
		scheduler, &mockFactory{}, logs.NewConfigurator(nil, zap.NewNop()), zap.NewNop())
	h.modesOut = out
	return h
}

// A host stopped while its program still runs never reaches the input
// reader's own cleanup, so the host itself must turn mouse reporting and
// bracketed paste off, or the shell the person returns to receives an SGR
// sequence on every mouse move.
func TestHost_StopTurnsTerminalModesOff(t *testing.T) {
	var out bytes.Buffer
	h := newModesHost(t, &out)

	_, err := h.Start(context.Background())
	require.NoError(t, err)
	require.NoError(t, h.Stop(context.Background()))

	written := out.String()
	for _, mode := range []string{"1000", "1002", "1003", "1006", "1015", "2004"} {
		assert.Contains(t, written, "\033[?"+mode+"l", "mode %s left on after Stop", mode)
	}
	assert.NotContains(t, written, "h", "Stop must only switch modes off")
}

// A host that is not running has no terminal to restore: a second Stop writes
// nothing.
func TestHost_StopTwiceWritesModesOnce(t *testing.T) {
	var out bytes.Buffer
	h := newModesHost(t, &out)

	_, err := h.Start(context.Background())
	require.NoError(t, err)
	require.NoError(t, h.Stop(context.Background()))
	first := out.Len()
	require.NoError(t, h.Stop(context.Background()))

	assert.Equal(t, first, out.Len())
	assert.Equal(t, len(terminalModesReset), first)
}

// Escape sequences go only to a terminal: a pipe (logs redirected to a file
// or another program) gets nothing.
func TestTerminalOut_OnlyATerminal(t *testing.T) {
	assert.Nil(t, terminalOut(nil))

	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	assert.Nil(t, terminalOut(writer))
}
