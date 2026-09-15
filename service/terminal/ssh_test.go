// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/attrs"
	ctxapi "github.com/wippyai/runtime/api/context"
	"github.com/wippyai/runtime/api/payload"
	"github.com/wippyai/runtime/api/process"
	"github.com/wippyai/runtime/api/registry"
	secapi "github.com/wippyai/runtime/api/security"
	terminalapi "github.com/wippyai/runtime/api/service/terminal"
	ttyapi "github.com/wippyai/runtime/api/tty"
	"github.com/wippyai/runtime/system/scheduler/actor"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
)

var sessionEntry = registry.NewID("test", "desk")

// sessionRegistry knows one entry, the one the host runs per connection.
type sessionRegistry struct {
	registry.Registry
	entries map[registry.ID]registry.Entry
}

func (r sessionRegistry) GetEntry(id registry.ID) (registry.Entry, error) {
	entry, ok := r.entries[id]
	if !ok {
		return registry.Entry{}, errors.New("entry not found")
	}
	return entry, nil
}

type sessionSight struct {
	terminal *terminalapi.PipeContext
	values   ctxapi.Values
	actor    secapi.Actor
}

// sessionProcess records what a session's program was given, and ends at
// once, waits, or waits and obeys the first message (a cancel).
type sessionProcess struct {
	seen     chan sessionSight
	messages chan struct{}
	failure  error
	// stepFailure fails the program after it started, not at its start.
	stepFailure error
	stay        bool
	obey        bool
}

func (p *sessionProcess) Init(ctx context.Context, _ string, _ payload.Payloads) error {
	actor, _ := secapi.GetActor(ctx)
	p.seen <- sessionSight{
		terminal: terminalapi.GetTerminalContext(ctx),
		values:   ctxapi.GetValues(ctx),
		actor:    actor,
	}
	return p.failure
}

func (p *sessionProcess) Step(events []process.Event, out *process.StepOutput) error {
	if p.stepFailure != nil {
		return p.stepFailure
	}
	for _, event := range events {
		if event.Type != process.EventMessage {
			continue
		}
		if p.messages != nil {
			select {
			case p.messages <- struct{}{}:
			default:
			}
		}
		if p.obey {
			out.Done(nil)
			return nil
		}
	}
	if p.stay {
		out.Idle()
		return nil
	}
	out.Done(nil)
	return nil
}

func (*sessionProcess) Close() {}

type sessionFactory struct{ proc process.Process }

func (f sessionFactory) Create(registry.ID) (process.Process, *process.Meta, error) {
	return f.proc, nil, nil
}

type sshFixture struct {
	host   *SSHHost
	signer ssh.Signer
	addr   string
}

func newClientSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(key)
	require.NoError(t, err)
	return signer
}

func startSSHHost(t *testing.T, proc process.Process, configure func(*terminalapi.SSHConfig)) *sshFixture {
	t.Helper()
	return startSSHHostWith(t, proc, configure, nil)
}

// startSSHHostWith is startSSHHost with a hook on the runtime's context — the
// place a test puts the function registry the host asks.
func startSSHHostWith(t *testing.T, proc process.Process, configure func(*terminalapi.SSHConfig),
	prepare func(context.Context) context.Context,
) *sshFixture {
	t.Helper()
	dir := t.TempDir()
	signer := newClientSigner(t)
	authorized := filepath.Join(dir, "authorized_keys")
	require.NoError(t, os.WriteFile(authorized, ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o600))

	cfg := &terminalapi.SSHConfig{
		Address:        "127.0.0.1:0",
		HostKey:        filepath.Join(dir, "host_key"),
		AuthorizedKeys: authorized,
		Entry:          sessionEntry.String(),
	}
	if configure != nil {
		configure(cfg)
	}
	require.NoError(t, cfg.Validate())

	appCtx := process.WithPIDGenerator(ctxapi.NewRootContext(), newTestPIDGen())
	appCtx = registry.WithRegistry(appCtx, sessionRegistry{entries: map[registry.ID]registry.Entry{
		sessionEntry: {ID: sessionEntry, Kind: "process.lua", Meta: attrs.NewBagFrom(map[string]any{
			"command": map[string]any{
				"name":     "desk",
				"security": map[string]any{"actor": map[string]any{"id": "test:desk-user"}},
			},
		})},
	}})
	if prepare != nil {
		appCtx = prepare(appCtx)
	}

	h := NewSSHHost(appCtx, registry.NewID("test", "ssh"), cfg, nil, sessionFactory{proc: proc}, zap.NewNop())
	h.dtt = jsonTranscoder{}
	h.scheduler = actor.NewScheduler(&mockCommandRegistry{},
		actor.WithWorkers(1),
		actor.WithLifecycle(&compositeLifecycle{host: h}),
	)
	_, err := h.Start(appCtx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	return &sshFixture{host: h, signer: signer, addr: h.Address().String()}
}

func (f *sshFixture) dial(signer ssh.Signer) (*ssh.Client, error) {
	return ssh.Dial("tcp", f.addr, &ssh.ClientConfig{
		User:            "alice",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
}

func (f *sshFixture) sessionCount() int {
	f.host.mu.Lock()
	defer f.host.mu.Unlock()
	return len(f.host.sessions)
}

type sessionOutput struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (b *sessionOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *sessionOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(what)
}

// openDesktop asks for a terminal and a shell, and answers the capability
// query the way a sixel terminal with 10x20 cells does.
func openDesktop(t *testing.T, client *ssh.Client) (*ssh.Session, *sessionOutput, *sessionOutput) {
	t.Helper()
	session, err := client.NewSession()
	require.NoError(t, err)
	stdin, err := session.StdinPipe()
	require.NoError(t, err)
	stdout, stderr := &sessionOutput{}, &sessionOutput{}
	session.Stdout, session.Stderr = stdout, stderr
	require.NoError(t, session.RequestPty("xterm-256color", 30, 100, ssh.TerminalModes{}))
	require.NoError(t, session.Shell())
	eventually(t, "the capability query never reached the client", func() bool {
		return strings.Contains(stdout.String(), probeQuery)
	})
	_, err = stdin.Write([]byte(sixelReply))
	require.NoError(t, err)
	return session, stdout, stderr
}

func receive(t *testing.T, seen chan sessionSight) sessionSight {
	t.Helper()
	select {
	case sight := <-seen:
		return sight
	case <-time.After(3 * time.Second):
		t.Fatal("the session's program never started")
		return sessionSight{}
	}
}

func TestSSHSessionRunsTheEntryOnItsOwnTerminal(t *testing.T) {
	proc := &sessionProcess{seen: make(chan sessionSight, 1)}
	f := startSSHHost(t, proc, nil)
	client, err := f.dial(f.signer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	session, _, _ := openDesktop(t, client)
	sight := receive(t, proc.seen)

	require.NotNil(t, sight.terminal, "the program must be attached to the connection's terminal")
	width, height, known := sight.terminal.Probe.CellSize()
	assert.True(t, known)
	assert.Equal(t, [2]int{10, 20}, [2]int{width, height})
	protocol, _ := sight.terminal.Probe.Detect()
	assert.Equal(t, ttyapi.GraphicsSixel, protocol)
	cols, rows, err := sight.terminal.Input.ScreenSize()
	require.NoError(t, err)
	assert.Equal(t, [2]int{100, 30}, [2]int{cols, rows})

	require.NotNil(t, sight.values)
	user, _ := sight.values.Get(SessionValueUser)
	assert.Equal(t, "alice", user)
	id, _ := sight.values.Get(SessionValueID)
	assert.NotEmpty(t, id)
	auth, _ := sight.values.Get(SessionValueAuth)
	assert.Equal(t, "key", auth, "an authorized key let this connection in")
	assert.Equal(t, "test:desk-user", sight.actor.ID, "meta.command.security applies, as for the CLI")

	require.NoError(t, session.Wait(), "a program that ended cleanly ends its connection with status 0")
	eventually(t, "the finished session was not forgotten", func() bool { return f.sessionCount() == 0 })
}

func TestSSHSessionReportsAFailedProgram(t *testing.T) {
	proc := &sessionProcess{seen: make(chan sessionSight, 1), failure: errors.New("no desk today")}
	f := startSSHHost(t, proc, nil)
	client, err := f.dial(f.signer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	session, _, stderr := openDesktop(t, client)
	err = session.Wait()
	var exit *ssh.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 1, exit.ExitStatus())
	assert.Contains(t, stderr.String(), "no desk today", "the person is told why, not only the log")
}

func TestSSHSessionReportsAProgramThatFailsLater(t *testing.T) {
	proc := &sessionProcess{seen: make(chan sessionSight, 1), stepFailure: errors.New("the desk fell over")}
	f := startSSHHost(t, proc, nil)
	client, err := f.dial(f.signer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	session, _, stderr := openDesktop(t, client)
	receive(t, proc.seen)
	err = session.Wait()
	var exit *ssh.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 1, exit.ExitStatus())
	assert.Contains(t, stderr.String(), "the desk fell over")
}

func TestSSHRefusesAKeyNotListed(t *testing.T) {
	proc := &sessionProcess{seen: make(chan sessionSight, 1)}
	f := startSSHHost(t, proc, nil)

	_, err := f.dial(newClientSigner(t))
	require.Error(t, err)
	assert.Empty(t, proc.seen)
}

func TestSSHRefusesASessionWithoutATerminal(t *testing.T) {
	proc := &sessionProcess{seen: make(chan sessionSight, 1)}
	f := startSSHHost(t, proc, nil)
	client, err := f.dial(f.signer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	// A raw channel: the library's Session drops stderr when the shell is
	// refused, where OpenSSH prints it — and what it says is the point.
	channel, requests, err := client.OpenChannel("session", nil)
	require.NoError(t, err)
	go ssh.DiscardRequests(requests)
	ok, err := channel.SendRequest("shell", true, nil)
	require.NoError(t, err)
	assert.False(t, ok)
	said, err := io.ReadAll(channel.Stderr())
	require.NoError(t, err)
	assert.Contains(t, string(said), "needs a terminal")
	assert.Empty(t, proc.seen, "no program runs without a terminal")
}

func TestSSHRefusesCommandsAndForwarding(t *testing.T) {
	proc := &sessionProcess{seen: make(chan sessionSight, 1)}
	f := startSSHHost(t, proc, nil)
	client, err := f.dial(f.signer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	session, err := client.NewSession()
	require.NoError(t, err)
	require.Error(t, session.Run("id"))

	// The refused command leaves the connection usable: a first attempt
	// that failed must not take it along.
	_, _, err = client.OpenChannel("direct-tcpip", ssh.Marshal(struct { //nolint:govet // fieldalignment: RFC 4254 §7.2 field order
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}{"127.0.0.1", 22, "127.0.0.1", 50000}))
	var refused *ssh.OpenChannelError
	require.ErrorAs(t, err, &refused, "the host must not become a way into the network behind it")
	assert.Equal(t, ssh.UnknownChannelType, refused.Reason)
	assert.Empty(t, proc.seen)
}

func TestSSHLimitsSessions(t *testing.T) {
	proc := &sessionProcess{seen: make(chan sessionSight, 2), stay: true}
	f := startSSHHost(t, proc, func(cfg *terminalapi.SSHConfig) {
		cfg.MaxSessions = 1
		cfg.CloseGrace = "100ms"
	})
	client, err := f.dial(f.signer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	openDesktop(t, client)
	receive(t, proc.seen)
	_, err = client.NewSession()
	require.Error(t, err)
}

func TestSSHHangupAsksTheProgramToFinish(t *testing.T) {
	proc := &sessionProcess{seen: make(chan sessionSight, 1), messages: make(chan struct{}, 1), stay: true, obey: true}
	f := startSSHHost(t, proc, func(cfg *terminalapi.SSHConfig) { cfg.CloseGrace = "30s" })
	client, err := f.dial(f.signer)
	require.NoError(t, err)

	openDesktop(t, client)
	receive(t, proc.seen)
	require.Equal(t, 1, f.sessionCount())
	require.NoError(t, client.Close())

	select {
	case <-proc.messages:
	case <-time.After(3 * time.Second):
		t.Fatal("the program was not asked to finish when its terminal left")
	}
	eventually(t, "the program that finished was not forgotten", func() bool { return f.sessionCount() == 0 })
}

func TestSSHHangupTerminatesAProgramThatStays(t *testing.T) {
	proc := &sessionProcess{seen: make(chan sessionSight, 1), stay: true}
	f := startSSHHost(t, proc, func(cfg *terminalapi.SSHConfig) { cfg.CloseGrace = "100ms" })
	client, err := f.dial(f.signer)
	require.NoError(t, err)

	openDesktop(t, client)
	receive(t, proc.seen)
	require.NoError(t, client.Close())

	eventually(t, "a program ignoring the cancel outlived its grace", func() bool { return f.sessionCount() == 0 })
}

func TestSSHHostKeyIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "host")
	first, err := loadHostKey(path)
	require.NoError(t, err)
	second, err := loadHostKey(path)
	require.NoError(t, err)
	assert.Equal(t, ssh.FingerprintSHA256(first.PublicKey()), ssh.FingerprintSHA256(second.PublicKey()),
		"clients must see the same host from one start to the next")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestSSHAuthorizedKeysSkipRestrictedKeys(t *testing.T) {
	plain, restricted := newClientSigner(t), newClientSigner(t)
	path := filepath.Join(t.TempDir(), "authorized_keys")
	content := string(ssh.MarshalAuthorizedKey(plain.PublicKey())) +
		`from="10.0.0.1" ` + string(ssh.MarshalAuthorizedKey(restricted.PublicKey()))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	keys, err := readAuthorizedKeys(path)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, plain.PublicKey().Marshal(), keys[0].Marshal(),
		"a key its owner restricted is not accepted without the restriction")
}
