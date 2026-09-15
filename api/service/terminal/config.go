// SPDX-License-Identifier: MPL-2.0

// Package terminal provides terminal service configuration.
package terminal

import (
	"errors"
	"fmt"
	"time"

	"github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/api/supervisor"
)

// Host identifies a terminal service component
const Host registry.Kind = "terminal.host"

type (
	// HostConfig represents the configuration for a terminal service
	HostConfig struct {
		Lifecycle supervisor.LifecycleConfig `json:"lifecycle"`
		HideLogs  bool                       `json:"hide_logs"`
	}
)

// SSH identifies a terminal host that serves remote terminals over SSH: each
// connection is a terminal of its own, and runs the configured entry on it.
const SSH registry.Kind = "terminal.ssh"

// Ways a terminal.ssh host lets a connection in.
const (
	// SSHAuthKeys admits the keys listed in authorized_keys.
	SSHAuthKeys = "keys"
	// SSHAuthLogon asks nothing at the door: the entry itself finds out who
	// came (the Windows shell's "Log On to Windows", against the
	// application's accounts). The session is marked as one nobody vouched
	// for, and an entry that cannot ask must refuse it.
	SSHAuthLogon = "logon"
)

// SSHConfig configures a terminal.ssh host.
type SSHConfig struct {
	// Address to listen on. Loopback by default: exposing a desktop to the
	// network is a decision, not a default.
	Address string `json:"address"`
	// HostKey is the server's private key. A missing file is generated once
	// (ed25519) and kept, so clients see the same host from one start to
	// the next.
	HostKey string `json:"host_key"`
	// Auth is how a connection is let in: SSHAuthKeys (the default) or
	// SSHAuthLogon.
	Auth string `json:"auth"`
	// AuthorizedKeys is an OpenSSH authorized_keys file, required with
	// auth: keys and refused with auth: logon. It is read on every attempt,
	// so a key added there works without a restart.
	AuthorizedKeys string `json:"authorized_keys"`
	// KeyOwner, with auth: logon, names a function that says whether an
	// offered public key is registered to an account ({key} → {known}). A
	// registered key the client proves it holds reaches the program as
	// terminal.key, and the program may log its owner on without a password;
	// every other client still gets in and meets the logon.
	KeyOwner string `json:"key_owner"`
	// Entry is the process each connection runs, "namespace:name". Its
	// meta.command.security applies, as it does for the CLI launcher.
	Entry string `json:"entry"`
	// CloseGrace is how long a process whose terminal disconnected has to
	// finish after it was asked to, before it is terminated.
	CloseGrace  string                     `json:"close_grace"`
	Lifecycle   supervisor.LifecycleConfig `json:"lifecycle"`
	MaxSessions int                        `json:"max_sessions"`
	Workers     int                        `json:"workers"`
	grace       time.Duration
}

// Grace is CloseGrace parsed; valid after Validate.
func (c *SSHConfig) Grace() time.Duration { return c.grace }

// Validate fills defaults and refuses a configuration that would serve
// nothing or serve anyone.
func (c *SSHConfig) Validate() error {
	c.Lifecycle.InitDefaults()
	if c.Address == "" {
		c.Address = "127.0.0.1:2222"
	}
	if c.HostKey == "" {
		c.HostKey = ".wippy/ssh_host_ed25519_key"
	}
	if c.MaxSessions <= 0 {
		c.MaxSessions = 8
	}
	if c.Workers <= 0 {
		c.Workers = 2
	}
	if c.CloseGrace == "" {
		c.CloseGrace = "10s"
	}
	grace, err := time.ParseDuration(c.CloseGrace)
	if err != nil || grace < 0 {
		return fmt.Errorf("terminal.ssh: close_grace %q is not a duration", c.CloseGrace)
	}
	c.grace = grace
	if c.Entry == "" {
		return errors.New("terminal.ssh: entry is required — the process each connection runs")
	}
	switch c.Auth {
	case "", SSHAuthKeys:
		c.Auth = SSHAuthKeys
		if c.AuthorizedKeys == "" {
			return errors.New("terminal.ssh: authorized_keys is required with auth: keys — " +
				"without it anyone who reaches the port would get a terminal " +
				"(auth: logon leaves that to the entry's own logon)")
		}
		if c.KeyOwner != "" {
			return errors.New("terminal.ssh: key_owner is for auth: logon — " +
				"with auth: keys the keys come from authorized_keys")
		}
	case SSHAuthLogon:
		// A key list that is read by nobody would look like a door that is
		// not there: the owner would believe only those keys get in.
		if c.AuthorizedKeys != "" {
			return errors.New("terminal.ssh: authorized_keys has no effect with auth: logon — " +
				"anyone is let in and the entry's logon decides; remove it or use auth: keys")
		}
	default:
		return fmt.Errorf("terminal.ssh: auth %q must be keys or logon", c.Auth)
	}
	return nil
}

// initDefaults initializes the HostConfig with default values
func (c *HostConfig) initDefaults() {
	c.Lifecycle.InitDefaults()
}

func (c *HostConfig) Validate() error {
	c.initDefaults()
	return nil
}
