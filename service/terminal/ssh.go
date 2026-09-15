// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wippyai/runtime/api/attrs"
	ctxapi "github.com/wippyai/runtime/api/context"
	dispatcherapi "github.com/wippyai/runtime/api/dispatcher"
	"github.com/wippyai/runtime/api/event"
	"github.com/wippyai/runtime/api/function"
	"github.com/wippyai/runtime/api/payload"
	"github.com/wippyai/runtime/api/pid"
	"github.com/wippyai/runtime/api/process"
	"github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/api/relay"
	"github.com/wippyai/runtime/api/runtime"
	secapi "github.com/wippyai/runtime/api/security"
	terminalapi "github.com/wippyai/runtime/api/service/terminal"
	"github.com/wippyai/runtime/api/supervisor"
	"github.com/wippyai/runtime/api/topology"
	ttyapi "github.com/wippyai/runtime/api/tty"
	entryutil "github.com/wippyai/runtime/system/entry"
	"github.com/wippyai/runtime/system/scheduler/actor"
	securitysys "github.com/wippyai/runtime/system/security"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
)

// A terminal host whose terminals are SSH connections.
//
// The ordinary terminal host serves the one terminal the process was started
// on, and its program ending ends the runtime. That makes a desktop something
// each person starts for themselves. Here the runtime keeps running and every
// connection gets a terminal of its own with the configured entry on it; a
// program ending ends that connection, nothing else.
//
// Everything a program learns about its terminal — size, resizes, what it can
// draw, how large a cell is — is learned per connection, because two people
// rarely sit at the same screen.

const (
	// sessionProbeTimeout bounds the wait for a remote terminal's answer to
	// the capability query. Longer than on the process's own terminal: the
	// answer crosses the network, and it is spent once per connection.
	sessionProbeTimeout = time.Second
	// sshHandshakeTimeout drops a connection that never finishes the
	// handshake, so a port scan does not hold a goroutine forever.
	sshHandshakeTimeout = 20 * time.Second
	sshMaxEnvVars       = 64
	sshMaxEnvValue      = 4096
)

// Values the session process finds with ctx.get. Inherited by what it spawns.
const (
	SessionValueID     = "terminal.session"
	SessionValueUser   = "terminal.user"
	SessionValueRemote = "terminal.remote"
	// SessionValueAuth says what let the connection in: "key" when an
	// authorized key did, "none" when nothing did (auth: logon) and the
	// program has to find out who came itself.
	SessionValueAuth = "terminal.auth"
	// SessionValueKey is the account key the client proved it holds, in its
	// canonical "type base64" form (auth: logon with key_owner). It says who
	// is coming, not that they may in: terminal.auth stays "none", and the
	// program asks the application to turn the key into a session.
	SessionValueKey = "terminal.key"
)

// keyOwnerTimeout bounds one key_owner call: it runs inside the handshake,
// and a function that hangs must not hold a connection forever.
const keyOwnerTimeout = 5 * time.Second

var errSessionsOnly = errors.New("a terminal.ssh host starts its processes itself, one per connection")

// SSHHost serves remote terminals over SSH.
type SSHHost struct {
	appCtx    context.Context
	ctx       context.Context
	listener  net.Listener
	server    *ssh.ServerConfig
	factory   process.Factory
	scheduler *actor.Scheduler
	// dtt decodes the key_owner function's answer.
	dtt      payload.Transcoder
	cfg      *terminalapi.SSHConfig
	log      *zap.Logger
	statusCh chan any
	sessions map[string]*sshSession
	conns    map[*sshConn]struct{}
	id       registry.ID
	wg       sync.WaitGroup
	mu       sync.Mutex
	slots    int
	running  atomic.Bool
	shutdown atomic.Bool
}

// NewSSHHost creates a host. appCtx is where the runtime's registry and pid
// generator are found; the host keeps it to start processes outside any
// caller's context.
func NewSSHHost(appCtx context.Context, id registry.ID, cfg *terminalapi.SSHConfig,
	scheduler *actor.Scheduler, factory process.Factory, logger *zap.Logger,
) *SSHHost {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &SSHHost{
		appCtx:    appCtx,
		id:        id,
		cfg:       cfg,
		scheduler: scheduler,
		factory:   factory,
		log:       logger,
		statusCh:  make(chan any, 1),
		sessions:  make(map[string]*sshSession),
		conns:     make(map[*sshConn]struct{}),
	}
}

// Address is where the host listens once started.
func (h *SSHHost) Address() net.Addr {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.listener == nil {
		return nil
	}
	return h.listener.Addr()
}

// Start implements supervisor.Service.
func (h *SSHHost) Start(ctx context.Context) (<-chan any, error) {
	if h.running.Swap(true) {
		return nil, ErrHostAlreadyRunning
	}
	signer, err := loadHostKey(h.cfg.HostKey)
	if err != nil {
		h.running.Store(false)
		return nil, fmt.Errorf("ssh host key %s: %w", h.cfg.HostKey, err)
	}
	server := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-wippy",
		MaxAuthTries:  6,
	}
	switch {
	case h.cfg.Auth == terminalapi.SSHAuthLogon && h.cfg.KeyOwner != "":
		// Keys first, so a registered one can log its owner on; anyone else
		// still reaches the logon through a keyboard-interactive round that
		// asks nothing. "none" is refused on purpose: a client told "none"
		// works never offers its keys. A client may carry many keys, and
		// every unregistered one is a failed attempt — hence more of them.
		server.PublicKeyCallback = h.accountKey
		server.KeyboardInteractiveCallback = func(ssh.ConnMetadata, ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			return nil, nil
		}
		server.MaxAuthTries = 20
	case h.cfg.Auth == terminalapi.SSHAuthLogon:
		// Nobody is asked at the door: the entry's own logon is the door,
		// and the session carries terminal.auth = "none" to say so.
		server.NoClientAuth = true
	default:
		server.PublicKeyCallback = h.authorize
	}
	server.AddHostKey(signer)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", h.cfg.Address)
	if err != nil {
		h.running.Store(false)
		return nil, fmt.Errorf("ssh listen on %s: %w", h.cfg.Address, err)
	}

	h.mu.Lock()
	h.ctx = ctx
	h.server = server
	h.listener = listener
	h.statusCh = make(chan any, 1)
	statusCh := h.statusCh
	h.mu.Unlock()
	h.shutdown.Store(false)

	h.scheduler.Start()
	h.wg.Add(1)
	go h.acceptLoop(listener)

	h.log.Info("ssh terminal host listening",
		zap.String("id", h.id.String()),
		zap.String("address", listener.Addr().String()),
		zap.String("entry", h.cfg.Entry),
		zap.String("auth", h.cfg.Auth),
		zap.String("host_key", ssh.FingerprintSHA256(signer.PublicKey())))
	return statusCh, nil
}

// Stop implements supervisor.Service. Connections are dropped: a runtime
// that is stopping has nowhere to keep a desktop.
func (h *SSHHost) Stop(ctx context.Context) error {
	if !h.running.Swap(false) {
		return nil
	}

	h.mu.Lock()
	listener := h.listener
	conns := make([]*sshConn, 0, len(h.conns))
	for conn := range h.conns {
		conns = append(conns, conn)
	}
	statusCh := h.statusCh
	h.mu.Unlock()

	if listener != nil {
		_ = listener.Close()
	}
	// Dropping a connection asks its program to finish, as any hangup does.
	// The host keeps delivering messages meanwhile: a desktop closing its
	// windows has to hear them go.
	// Once the connection is gone nothing reaches the client's terminal, and
	// its program's own cleanup would come too late: switch the input modes
	// off while the channels are still open.
	for _, session := range h.openSessions() {
		session.restoreModes()
	}
	for _, conn := range conns {
		_ = conn.server.Close()
	}
	h.awaitSessions(ctx, h.cfg.Grace())
	for _, processID := range h.sessionPIDs() {
		_ = h.scheduler.Terminate(processID)
	}

	h.shutdown.Store(true)
	h.scheduler.Stop(ctx)

	done := make(chan struct{})
	go func() { h.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	close(statusCh)
	h.log.Info("ssh terminal host stopped", zap.String("id", h.id.String()))
	return nil
}

// openSessions is a snapshot of the sessions a program runs in.
func (h *SSHHost) openSessions() []*sshSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	sessions := make([]*sshSession, 0, len(h.sessions))
	for _, session := range h.sessions {
		sessions = append(sessions, session)
	}
	return sessions
}

func (h *SSHHost) sessionPIDs() []pid.PID {
	h.mu.Lock()
	defer h.mu.Unlock()
	pids := make([]pid.PID, 0, len(h.sessions))
	for _, session := range h.sessions {
		session.mu.Lock()
		pids = append(pids, session.processID)
		session.mu.Unlock()
	}
	return pids
}

// awaitSessions waits for every session's program to end, for at most the
// grace or until ctx is done.
func (h *SSHHost) awaitSessions(ctx context.Context, grace time.Duration) {
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		h.mu.Lock()
		left := len(h.sessions)
		h.mu.Unlock()
		if left == 0 {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// Run implements process.Host. Nobody else starts processes here: a process
// on this host without a connection would have no terminal.
func (h *SSHHost) Run(context.Context, *process.Start) (pid.PID, error) {
	return pid.PID{}, errSessionsOnly
}

// Terminate implements process.Host.
func (h *SSHHost) Terminate(_ context.Context, processID pid.PID) error {
	return h.scheduler.Terminate(processID)
}

// Send implements relay.Receiver.
func (h *SSHHost) Send(pkg *relay.Package) error {
	return h.SendContext(context.Background(), pkg)
}

// SendContext implements relay.ContextSender.
func (h *SSHHost) SendContext(ctx context.Context, pkg *relay.Package) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.shutdown.Load() {
		return ErrHostShuttingDown
	}
	return h.scheduler.SendContext(ctx, pkg)
}

// OnStart implements process.Lifecycle.
func (h *SSHHost) OnStart(context.Context, pid.PID, process.Process) error { return nil }

// OnComplete implements process.Lifecycle: the program is gone, and so is
// its connection.
func (h *SSHHost) OnComplete(ctx context.Context, processID pid.PID, result *runtime.Result) {
	if tc := terminalapi.GetTerminalContext(ctx); tc != nil && tc.Input != nil {
		_ = tc.Input.Stop()
	}
	// Closing the frame closes the surface, which takes the remote screen
	// out of the alternate buffer — before the channel goes, not after.
	if fc := ctxapi.FrameFromContext(ctx); fc != nil {
		_ = fc.Close()
	}

	h.mu.Lock()
	session := h.sessions[processID.String()]
	delete(h.sessions, processID.String())
	h.mu.Unlock()
	if session == nil {
		return
	}
	code, output := completionExitCode(result)
	message := output
	if result != nil && result.Error != nil {
		message = "wippy: " + result.Error.Error()
	}
	fields := []zap.Field{
		zap.String("pid", processID.String()),
		zap.String("user", session.conn.user), zap.String("remote", session.conn.remote),
		zap.Int("exit_code", code),
	}
	// The terminal that would have shown the error may be gone: the log is
	// the only place left to say it.
	if result != nil && result.Error != nil {
		fields = append(fields, zap.String("error", result.Error.Error()))
	}
	h.log.Info("ssh session ended", fields...)
	go session.finish(code, message)
}

func (h *SSHHost) acceptLoop(listener net.Listener) {
	defer h.wg.Done()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if h.shutdown.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			h.log.Warn("ssh accept failed", zap.Error(err))
			time.Sleep(50 * time.Millisecond)
			continue
		}
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			h.serveConn(conn)
		}()
	}
}

// sshConn is one authenticated connection; it may carry several sessions,
// one after another or at once (OpenSSH multiplexing). The client closes it:
// a refused first attempt must not take the connection with it.
type sshConn struct {
	server      *ssh.ServerConn
	user        string
	remote      string
	fingerprint string
	// vouched is SessionValueAuth: "key" or "none".
	vouched string
	// accountKey is SessionValueKey, empty when no account key was proven.
	accountKey string
}

func (h *SSHHost) serveConn(raw net.Conn) {
	if tcp, ok := raw.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(30 * time.Second)
	}
	_ = raw.SetDeadline(time.Now().Add(sshHandshakeTimeout))
	server, channels, requests, err := ssh.NewServerConn(raw, h.server)
	if err != nil {
		h.log.Info("ssh connection refused",
			zap.String("remote", raw.RemoteAddr().String()), zap.Error(err))
		_ = raw.Close()
		return
	}
	_ = raw.SetDeadline(time.Time{})

	conn := &sshConn{server: server, user: server.User(), remote: server.RemoteAddr().String(), vouched: "none"}
	if server.Permissions != nil {
		conn.fingerprint = server.Permissions.Extensions["fingerprint"]
		conn.accountKey = server.Permissions.Extensions["account_key"]
	}
	// Only a key from authorized_keys vouches for the connection. A key
	// registered to an account says who is coming, and the program still
	// decides whether to let them in.
	if conn.fingerprint != "" && h.cfg.Auth != terminalapi.SSHAuthLogon {
		conn.vouched = "key"
	}
	h.mu.Lock()
	h.conns[conn] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.conns, conn)
		h.mu.Unlock()
		_ = server.Close()
	}()
	h.log.Info("ssh connection accepted",
		zap.String("user", conn.user), zap.String("remote", conn.remote),
		zap.String("auth", conn.vouched), zap.String("key", conn.fingerprint))

	// Global requests are port forwarding and keepalives. Forwarding is
	// refused: this host hands out terminals, not a way into the network
	// behind it.
	go ssh.DiscardRequests(requests)
	for incoming := range channels {
		if incoming.ChannelType() != "session" {
			_ = incoming.Reject(ssh.UnknownChannelType, "this server only opens terminal sessions")
			continue
		}
		if !h.takeSlot() {
			_ = incoming.Reject(ssh.ResourceShortage, "too many terminal sessions are open")
			continue
		}
		channel, channelRequests, err := incoming.Accept()
		if err != nil {
			h.releaseSlot()
			continue
		}
		session := &sshSession{host: h, conn: conn, channel: channel, env: make(map[string]string)}
		go h.serveSession(session, channelRequests)
	}
}

func (h *SSHHost) takeSlot() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.slots >= h.cfg.MaxSessions {
		return false
	}
	h.slots++
	return true
}

func (h *SSHHost) releaseSlot() {
	h.mu.Lock()
	h.slots--
	h.mu.Unlock()
}

// Payloads of the channel requests a terminal session uses (RFC 4254 §6).
// Field order is the wire order ssh.Unmarshal reads, not a choice.
type (
	ptyRequest struct { //nolint:govet // fieldalignment: RFC 4254 §6.2 field order
		Term    string
		Columns uint32
		Rows    uint32
		Width   uint32
		Height  uint32
		Modes   string
	}
	windowChange struct {
		Columns uint32
		Rows    uint32
		Width   uint32
		Height  uint32
	}
	envRequest struct {
		Name  string
		Value string
	}
	exitStatus struct {
		Status uint32
	}
)

func (h *SSHHost) serveSession(session *sshSession, requests <-chan *ssh.Request) {
	defer func() {
		session.hangup()
		h.releaseSlot()
	}()
	for request := range requests {
		switch request.Type {
		case "pty-req":
			var pty ptyRequest
			if ssh.Unmarshal(request.Payload, &pty) != nil {
				_ = request.Reply(false, nil)
				continue
			}
			session.setPty(pty)
			_ = request.Reply(true, nil)
		case "window-change":
			var change windowChange
			if ssh.Unmarshal(request.Payload, &change) == nil {
				session.resize(int(change.Columns), int(change.Rows), int(change.Width), int(change.Height))
			}
		case "env":
			var variable envRequest
			accepted := ssh.Unmarshal(request.Payload, &variable) == nil && session.setEnv(variable.Name, variable.Value)
			_ = request.Reply(accepted, nil)
		case "shell":
			if refusal := session.begin(); refusal != "" {
				_, _ = session.channel.Stderr().Write([]byte("wippy: " + refusal + "\r\n"))
				_ = request.Reply(false, nil)
				_ = session.channel.Close()
				continue
			}
			_ = request.Reply(true, nil)
			go session.start()
		case "exec", "subsystem":
			_, _ = session.channel.Stderr().Write([]byte(
				"wippy: this server shows a desktop and runs no commands: connect without one\r\n"))
			_ = request.Reply(false, nil)
			_ = session.channel.Close()
		default:
			if request.WantReply {
				_ = request.Reply(false, nil)
			}
		}
	}
}

// sshSession is one terminal: a session channel and the process on it.
type sshSession struct {
	host      *SSHHost
	conn      *sshConn
	channel   ssh.Channel
	env       map[string]string
	terminal  *sessionTerminal
	processID pid.PID
	term      string
	mu        sync.Mutex
	cols      int
	rows      int
	pixelW    int
	pixelH    int
	hasPty    bool
	started   bool
	running   bool
	hungUp    bool
	finished  bool
}

func (s *sshSession) setPty(pty ptyRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hasPty = true
	s.term = pty.Term
	s.cols, s.rows = int(pty.Columns), int(pty.Rows)
	s.pixelW, s.pixelH = int(pty.Width), int(pty.Height)
}

func (s *sshSession) resize(cols, rows, pixelW, pixelH int) {
	s.mu.Lock()
	s.cols, s.rows, s.pixelW, s.pixelH = cols, rows, pixelW, pixelH
	terminal := s.terminal
	s.mu.Unlock()
	if terminal != nil {
		terminal.resize(cols, rows, pixelW, pixelH)
	}
}

func (s *sshSession) setEnv(name, value string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || name == "" || len(value) > sshMaxEnvValue || len(s.env) >= sshMaxEnvVars {
		return false
	}
	s.env[name] = value
	return true
}

// begin claims the session for a program, or says why it cannot have one.
func (s *sshSession) begin() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return "this session already runs a program"
	}
	if !s.hasPty {
		return "this server shows a desktop and needs a terminal: connect with ssh -t"
	}
	s.started = true
	return ""
}

func (s *sshSession) start() {
	out := &onlcrWriter{w: s.channel}
	errOut := &onlcrWriter{w: s.channel.Stderr()}

	s.mu.Lock()
	terminal := newSessionTerminal(s.channel, s.term, s.env)
	terminal.cols, terminal.rows = s.cols, s.rows
	terminal.pixelW, terminal.pixelH = s.pixelW, s.pixelH
	s.terminal = terminal
	s.mu.Unlock()

	terminal.ask(out, sessionProbeTimeout)
	protocol, _ := terminal.probe.Detect()
	cellW, cellH, _ := terminal.probe.CellSize()

	processID, err := s.host.spawn(s, terminal, out, errOut)
	if err != nil {
		s.host.log.Warn("ssh session did not start",
			zap.String("user", s.conn.user), zap.String("remote", s.conn.remote), zap.Error(err))
		s.finish(1, "wippy: the session could not start: "+err.Error())
		return
	}
	s.host.log.Info("ssh session started",
		zap.String("pid", processID.String()),
		zap.String("user", s.conn.user), zap.String("remote", s.conn.remote),
		zap.String("term", s.term), zap.String("graphics", protocol),
		zap.Int("cell_w", cellW), zap.Int("cell_h", cellH))
}

// hangup is the client going away while its program may still run. The
// program is asked to finish — a desktop closes its windows — and terminated
// if it has not after the grace.
func (s *sshSession) hangup() {
	s.mu.Lock()
	s.hungUp = true
	running, processID := s.running, s.processID
	s.mu.Unlock()
	if running {
		s.host.log.Info("ssh terminal left; asking its program to finish",
			zap.String("pid", processID.String()),
			zap.String("user", s.conn.user), zap.String("remote", s.conn.remote))
		s.host.cancel(processID)
	}
}

// restoreModes switches the client terminal's input modes (mouse reporting,
// bracketed paste) back off. OpenSSH restores its own raw mode when the
// session ends, but not modes the program switched on: without this, every
// mouse move in the shell the person returns to types an SGR sequence.
func (s *sshSession) restoreModes() {
	s.mu.Lock()
	pty, finished := s.hasPty, s.finished
	s.mu.Unlock()
	if !pty || finished {
		return
	}
	_, _ = s.channel.Write([]byte(terminalModesReset))
}

// finish reports how the program ended and closes the channel.
func (s *sshSession) finish(code int, message string) {
	s.restoreModes()
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.running = false
	terminal := s.terminal
	s.mu.Unlock()

	if message != "" {
		_, _ = (&onlcrWriter{w: s.channel.Stderr()}).Write([]byte(message + "\n"))
	}
	_, _ = s.channel.SendRequest("exit-status", false, ssh.Marshal(exitStatus{Status: uint32(code)}))
	_ = s.channel.Close()
	if terminal != nil {
		terminal.close()
	}
}

// spawn starts the configured entry on the session's terminal.
func (h *SSHHost) spawn(session *sshSession, terminal *sessionTerminal, out, errOut *onlcrWriter) (pid.PID, error) {
	if !h.running.Load() || h.shutdown.Load() {
		return pid.PID{}, ErrHostShuttingDown
	}
	source := registry.ParseID(h.cfg.Entry)
	generator := process.GetPIDGenerator(h.appCtx)
	if generator == nil {
		return pid.PID{}, errors.New("pid generator not available")
	}
	security, err := commandSecurityPairs(h.appCtx, source)
	if err != nil {
		return pid.PID{}, err
	}
	proc, meta, err := h.factory.Create(source)
	if err != nil {
		return pid.PID{}, err
	}
	processID := generator.Generate(h.id.String())

	tc := terminalapi.NewTerminalContextWithArgs(terminal, out, errOut, nil)
	tc.Raw = &sessionRaw{}
	tc.Probe = terminal.probe
	tc.Input = newStreamInputReader(terminal, out, h.scheduler, processID)
	tc.Surface = func(options ttyapi.SurfaceOptions) (ttyapi.Surface, error) {
		return NewProbedSurface(out, options, terminal.probe), nil
	}
	sessionValues := map[string]any{
		SessionValueID:     processID.UniqID,
		SessionValueUser:   session.conn.user,
		SessionValueRemote: session.conn.remote,
		SessionValueAuth:   session.conn.vouched,
	}
	if session.conn.accountKey != "" {
		sessionValues[SessionValueKey] = session.conn.accountKey
	}
	values := attrs.NewBagFrom(sessionValues)

	var start process.Start
	frameCtx, fc := ctxapi.OpenFrameContextOn(h.ctx, h.appCtx)
	pairs := []ctxapi.Pair{
		{Key: runtime.FrameIDKey, Value: source},
		{Key: runtime.FramePIDKey, Value: processID},
		{Key: runtime.FrameLifecycleOptionsKey, Value: start.Options},
		{Key: terminalapi.Key(), Value: tc},
		ctxapi.ValuesPair(values),
	}
	pairs = append(pairs, security...)
	if err := fc.SetMultiple(pairs...); err != nil {
		proc.Close()
		ctxapi.ReleaseFrameContext(fc)
		return pid.PID{}, fmt.Errorf("set frame context: %w", err)
	}
	if meta != nil && meta.Security != nil {
		frameCtx, err = securitysys.WithSecurityConfigE(frameCtx, meta.Security)
		if err != nil {
			proc.Close()
			ctxapi.ReleaseFrameContext(fc)
			return pid.PID{}, fmt.Errorf("resolve process security: %w", err)
		}
	}
	method := "main"
	if meta != nil && meta.Method != "" {
		method = meta.Method
	}

	// Registered before it runs: a program that ends at once completes
	// before Submit returns, and its connection must still be found.
	session.mu.Lock()
	session.processID = processID
	session.running = true
	hungUp := session.hungUp
	session.mu.Unlock()
	h.mu.Lock()
	h.sessions[processID.String()] = session
	h.mu.Unlock()

	if _, err := h.scheduler.Submit(frameCtx, processID, proc, method, payload.Payloads(nil)); err != nil {
		h.mu.Lock()
		delete(h.sessions, processID.String())
		h.mu.Unlock()
		session.mu.Lock()
		session.running = false
		session.mu.Unlock()
		proc.Close()
		ctxapi.ReleaseFrameContext(fc)
		return pid.PID{}, err
	}
	// The client left while the program was being started.
	if hungUp {
		h.cancel(processID)
	}
	return processID, nil
}

// cancel asks a session's program to finish, and terminates it after the
// grace if it has not.
func (h *SSHHost) cancel(processID pid.PID) {
	_ = h.scheduler.Send(topology.CancelPackage(topology.SystemPID, processID, "the terminal disconnected"))
	time.AfterFunc(h.cfg.Grace(), func() {
		h.mu.Lock()
		_, alive := h.sessions[processID.String()]
		h.mu.Unlock()
		if alive {
			h.log.Info("ssh session did not finish after its terminal left; terminating",
				zap.String("pid", processID.String()))
			_ = h.scheduler.Terminate(processID)
		}
	})
}

// authorize accepts a key listed in the authorized_keys file. The file is
// read on every attempt, so adding a person does not need a restart.
func (h *SSHHost) authorize(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	fingerprint := ssh.FingerprintSHA256(key)
	keys, err := readAuthorizedKeys(h.cfg.AuthorizedKeys)
	if err != nil {
		h.log.Warn("ssh authorized keys unreadable",
			zap.String("path", h.cfg.AuthorizedKeys), zap.Error(err))
		return nil, errors.New("authorized keys unreadable")
	}
	wire := key.Marshal()
	for _, allowed := range keys {
		if bytes.Equal(allowed.Marshal(), wire) {
			return &ssh.Permissions{Extensions: map[string]string{"fingerprint": fingerprint}}, nil
		}
	}
	h.log.Info("ssh key not authorized",
		zap.String("user", meta.User()), zap.String("remote", meta.RemoteAddr().String()),
		zap.String("key", fingerprint))
	return nil, fmt.Errorf("key %s is not authorized", fingerprint)
}

// accountKey accepts an offered key only if the application says it is
// registered to an account. The canonical key rides in the permissions, and
// x/crypto applies those only after the client proves it holds the private
// half — so terminal.key is always a key its bearer owns.
func (h *SSHHost) accountKey(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	canonical := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	fingerprint := ssh.FingerprintSHA256(key)
	known, owner, err := h.askKeyOwner(canonical)
	if err != nil {
		// A lookup that failed costs a password prompt, not a way in.
		h.log.Warn("ssh key owner lookup failed; the key is treated as unknown",
			zap.String("function", h.cfg.KeyOwner), zap.String("key", fingerprint), zap.Error(err))
		return nil, errors.New("key lookup failed")
	}
	if !known {
		return nil, fmt.Errorf("key %s is not registered", fingerprint)
	}
	h.log.Info("ssh account key offered",
		zap.String("user", meta.User()), zap.String("remote", meta.RemoteAddr().String()),
		zap.String("key", fingerprint), zap.String("account", owner))
	return &ssh.Permissions{Extensions: map[string]string{"fingerprint": fingerprint, "account_key": canonical}}, nil
}

// askKeyOwner calls the key_owner function: {key} → {known, user_id, error}.
func (h *SSHHost) askKeyOwner(key string) (bool, string, error) {
	functions := function.GetRegistry(h.appCtx)
	if functions == nil {
		return false, "", errors.New("function registry not available")
	}
	if h.dtt == nil {
		return false, "", errors.New("payload transcoder not available")
	}
	base := h.ctx
	if base == nil {
		base = h.appCtx
	}
	ctx, cancel := context.WithTimeout(base, keyOwnerTimeout)
	defer cancel()
	callCtx, fc := ctxapi.OpenFrameContextOn(ctx, h.appCtx)
	defer ctxapi.ReleaseFrameContext(fc)
	result, err := functions.Call(callCtx, runtime.Task{
		ID:       registry.ParseID(h.cfg.KeyOwner),
		Payloads: payload.Payloads{payload.New(map[string]any{"key": key})},
	})
	if err != nil {
		return false, "", err
	}
	if result == nil || result.Value == nil {
		return false, "", errors.New("the function gave no answer")
	}
	if result.Error != nil {
		return false, "", result.Error
	}
	var answer struct {
		UserID string `json:"user_id"`
		Error  string `json:"error"`
		Known  bool   `json:"known"`
	}
	if err := h.dtt.Unmarshal(result.Value, &answer); err != nil {
		return false, "", fmt.Errorf("decode the answer: %w", err)
	}
	if !answer.Known && answer.Error != "" {
		return false, "", errors.New(answer.Error)
	}
	return answer.Known, answer.UserID, nil
}

// readAuthorizedKeys reads an OpenSSH authorized_keys file.
//
// A key carrying options (from=, command=, restrict…) is left out, not
// accepted without them: honoring none of the options would turn a key its
// owner restricted into one that is not.
func readAuthorizedKeys(path string) ([]ssh.PublicKey, error) {
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return nil, err
	}
	var keys []ssh.PublicKey
	rest := data
	for len(bytes.TrimSpace(rest)) > 0 {
		key, _, options, next, err := ssh.ParseAuthorizedKey(rest)
		if err != nil {
			break
		}
		if len(options) == 0 {
			keys = append(keys, key)
		}
		rest = next
	}
	return keys, nil
}

// loadHostKey reads the server's private key, generating and keeping one on
// first start.
func loadHostKey(path string) (ssh.Signer, error) {
	path = expandHome(path)
	data, err := os.ReadFile(path)
	if err == nil {
		return ssh.ParsePrivateKey(data)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(private, "wippy terminal.ssh host key")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(pem.EncodeToMemory(block)); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(private)
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

// commandSecurityPairs resolves meta.command.security of the entry, as the
// CLI launcher does. The same trust holds: the operator named this entry in
// the host's configuration on their own deployment.
func commandSecurityPairs(ctx context.Context, source registry.ID) ([]ctxapi.Pair, error) {
	reg := registry.GetRegistry(ctx)
	if reg == nil {
		return nil, errors.New("registry not available")
	}
	entry, err := reg.GetEntry(source)
	if err != nil {
		return nil, fmt.Errorf("session entry %s: %w", source.String(), err)
	}
	command, ok := entry.Meta["command"]
	if !ok {
		return nil, nil
	}
	encoded, err := json.Marshal(command)
	if err != nil {
		return nil, fmt.Errorf("encode command metadata: %w", err)
	}
	var declared struct {
		Security *secapi.Config `json:"security"`
	}
	if err := json.Unmarshal(encoded, &declared); err != nil {
		return nil, fmt.Errorf("decode command metadata: %w", err)
	}
	if declared.Security == nil {
		return nil, nil
	}
	return securitysys.ResolveConfigPairs(ctx, declared.Security)
}

// SSHManager manages terminal.ssh hosts.
type SSHManager struct {
	bus             event.Bus
	dtt             payload.Transcoder
	commandRegistry dispatcherapi.Registry
	factory         process.Factory
	log             *zap.Logger
	hosts           map[registry.ID]*SSHHost
	mu              sync.Mutex
}

// NewSSHManager creates the listener for terminal.ssh entries.
func NewSSHManager(bus event.Bus, dtt payload.Transcoder, cmdRegistry dispatcherapi.Registry,
	factory process.Factory, logger *zap.Logger,
) *SSHManager {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &SSHManager{
		bus: bus, dtt: dtt, commandRegistry: cmdRegistry, factory: factory,
		log: logger, hosts: make(map[registry.ID]*SSHHost),
	}
}

// Add implements registry.EntryListener.
func (m *SSHManager) Add(ctx context.Context, entry registry.Entry) error {
	cfg, err := entryutil.DecodeEntryConfig[terminalapi.SSHConfig](ctx, m.dtt, entry)
	if err != nil {
		return NewDecodeConfigError(err)
	}
	h := NewSSHHost(ctx, entry.ID, cfg, nil, m.factory, m.log.Named("ssh"))
	h.dtt = m.dtt
	h.scheduler = actor.NewScheduler(m.commandRegistry,
		actor.WithWorkers(cfg.Workers),
		actor.WithLifecycle(&compositeLifecycle{global: process.GetLifecycleRegistry(ctx), host: h}),
	)

	m.mu.Lock()
	m.hosts[entry.ID] = h
	m.mu.Unlock()

	m.bus.Send(ctx, event.Event{
		System: relay.System,
		Kind:   relay.HostRegister,
		Path:   entry.ID.String(),
		Data:   relay.Receiver(h),
	})
	m.bus.Send(ctx, event.Event{
		System: supervisor.System,
		Kind:   supervisor.ServiceRegister,
		Path:   entry.ID.String(),
		Data:   &supervisor.Entry{Service: h, Config: cfg.Lifecycle},
	})
	m.log.Info("ssh terminal host added", zap.String("id", entry.ID.String()))
	return nil
}

// Update implements registry.EntryListener.
func (m *SSHManager) Update(ctx context.Context, entry registry.Entry) error {
	if err := m.Delete(ctx, entry); err != nil {
		return err
	}
	return m.Add(ctx, entry)
}

// Delete implements registry.EntryListener.
func (m *SSHManager) Delete(ctx context.Context, entry registry.Entry) error {
	m.mu.Lock()
	if _, ok := m.hosts[entry.ID]; !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.hosts, entry.ID)
	m.mu.Unlock()

	m.bus.Send(ctx, event.Event{
		System: supervisor.System,
		Kind:   supervisor.ServiceRemove,
		Path:   entry.ID.String(),
	})
	m.bus.Send(ctx, event.Event{
		System: relay.System,
		Kind:   relay.HostDelete,
		Path:   entry.ID.String(),
	})
	m.log.Info("ssh terminal host deleted", zap.String("id", entry.ID.String()))
	return nil
}

var (
	_ process.Host       = (*SSHHost)(nil)
	_ supervisor.Service = (*SSHHost)(nil)
	_ process.Lifecycle  = (*SSHHost)(nil)
)
