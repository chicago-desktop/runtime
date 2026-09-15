// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	terminalapi "github.com/wippyai/runtime/api/service/terminal"
)

func modesOff(output string) bool {
	for _, mode := range []string{"1000", "1002", "1003", "1006", "1015", "2004"} {
		if !strings.Contains(output, "\033[?"+mode+"l") {
			return false
		}
	}
	return true
}

// The runtime stopping with a desktop open drops the connection; the client's
// terminal must get the input modes switched off before that, or the person's
// shell receives an SGR sequence on every mouse move.
func TestSSHStopTurnsTheClientsModesOff(t *testing.T) {
	proc := &sessionProcess{seen: make(chan sessionSight, 1), stay: true}
	f := startSSHHost(t, proc, func(cfg *terminalapi.SSHConfig) { cfg.CloseGrace = "100ms" })
	client, err := f.dial(f.signer)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	_, stdout, _ := openDesktop(t, client)
	receive(t, proc.seen)
	require.False(t, modesOff(stdout.String()), "the modes were switched off before anything asked")

	require.NoError(t, f.host.Stop(context.Background()))
	eventually(t, "the client's terminal kept mouse reporting after the host stopped", func() bool {
		return modesOff(stdout.String())
	})
}

// A program that ends by itself also leaves the client's terminal clean.
func TestSSHFinishedProgramTurnsTheClientsModesOff(t *testing.T) {
	proc := &sessionProcess{seen: make(chan sessionSight, 1)}
	f := startSSHHost(t, proc, nil)
	client, err := f.dial(f.signer)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	session, stdout, _ := openDesktop(t, client)
	receive(t, proc.seen)
	_ = session.Wait()
	require.True(t, modesOff(stdout.String()), "the client's terminal kept mouse reporting after the program ended")
}
