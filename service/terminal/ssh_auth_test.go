// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	terminalapi "github.com/wippyai/runtime/api/service/terminal"
	"golang.org/x/crypto/ssh"
)

func TestSSHLogonAuthLetsAnyoneInAndSaysNobodyVouched(t *testing.T) {
	proc := &sessionProcess{seen: make(chan sessionSight, 1)}
	f := startSSHHost(t, proc, func(cfg *terminalapi.SSHConfig) {
		cfg.Auth = terminalapi.SSHAuthLogon
		cfg.AuthorizedKeys = ""
	})

	// No key and no password: the door asks nothing.
	client, err := ssh.Dial("tcp", f.addr, &ssh.ClientConfig{
		User:            "bob",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	require.NoError(t, err, "with auth: logon nobody is asked at the door")
	t.Cleanup(func() { _ = client.Close() })

	session, _, _ := openDesktop(t, client)
	sight := receive(t, proc.seen)
	require.NotNil(t, sight.values)
	auth, _ := sight.values.Get(SessionValueAuth)
	assert.Equal(t, "none", auth, "the program must know that nobody vouched for this person")
	user, _ := sight.values.Get(SessionValueUser)
	assert.Equal(t, "bob", user)
	require.NoError(t, session.Wait())
}

func TestSSHConfigAuth(t *testing.T) {
	validate := func(auth, authorizedKeys string) (*terminalapi.SSHConfig, error) {
		cfg := &terminalapi.SSHConfig{Entry: "test:desk", Auth: auth, AuthorizedKeys: authorizedKeys}
		return cfg, cfg.Validate()
	}

	cfg, err := validate("", "keys")
	require.NoError(t, err)
	assert.Equal(t, terminalapi.SSHAuthKeys, cfg.Auth, "keys are the default")

	_, err = validate("keys", "")
	assert.ErrorContains(t, err, "authorized_keys is required")

	_, err = validate("logon", "")
	assert.NoError(t, err, "the entry's logon is the door")

	_, err = validate("logon", "~/.ssh/authorized_keys")
	assert.ErrorContains(t, err, "no effect", "a key list nobody reads must not look like a door")

	_, err = validate("password", "")
	assert.ErrorContains(t, err, "must be keys or logon")
}
