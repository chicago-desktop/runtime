// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/function"
	"github.com/wippyai/runtime/api/payload"
	"github.com/wippyai/runtime/api/runtime"
	terminalapi "github.com/wippyai/runtime/api/service/terminal"
	"golang.org/x/crypto/ssh"
)

const keyOwnerFunction = "test:key_owner"

// keyOwners stands in for the application's key_owner function.
type keyOwners struct {
	fail   error
	owners map[string]string
	asked  []string
	mu     sync.Mutex
}

func (k *keyOwners) Call(_ context.Context, task runtime.Task) (*runtime.Result, error) {
	if task.ID.String() != keyOwnerFunction {
		return nil, errors.New("unexpected function " + task.ID.String())
	}
	args, _ := task.Payloads[0].Data().(map[string]any)
	key, _ := args["key"].(string)
	k.mu.Lock()
	k.asked = append(k.asked, key)
	k.mu.Unlock()
	if k.fail != nil {
		return nil, k.fail
	}
	owner, known := k.owners[key]
	return &runtime.Result{Value: payload.New(map[string]any{"known": known, "user_id": owner})}, nil
}

// jsonTranscoder decodes a Go-valued payload the way the runtime's transcoder
// decodes a Lua one: by field names.
type jsonTranscoder struct{}

func (jsonTranscoder) Unmarshal(p payload.Payload, out any) error {
	data, err := json.Marshal(p.Data())
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (jsonTranscoder) Transcode(p payload.Payload, _ payload.Format) (payload.Payload, error) {
	return p, nil
}

func canonicalKey(signer ssh.Signer) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

func startAccountKeyHost(t *testing.T, proc *sessionProcess, owners *keyOwners) *sshFixture {
	t.Helper()
	return startSSHHostWith(t, proc, func(cfg *terminalapi.SSHConfig) {
		cfg.Auth = terminalapi.SSHAuthLogon
		cfg.AuthorizedKeys = ""
		cfg.KeyOwner = keyOwnerFunction
	}, func(ctx context.Context) context.Context {
		return function.WithRegistry(ctx, owners)
	})
}

// dialAs connects offering the signers in order, then a keyboard-interactive
// round that answers nothing — what a client without a registered key ends on.
func dialAs(t *testing.T, addr string, signers ...ssh.Signer) *ssh.Client {
	t.Helper()
	var auth []ssh.AuthMethod
	if len(signers) > 0 {
		auth = append(auth, ssh.PublicKeys(signers...))
	}
	auth = append(auth, ssh.KeyboardInteractive(func(string, string, []string, []bool) ([]string, error) {
		return nil, nil
	}))
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "carol",
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func sessionKey(t *testing.T, sight sessionSight) (any, bool) {
	t.Helper()
	require.NotNil(t, sight.values)
	auth, _ := sight.values.Get(SessionValueAuth)
	assert.Equal(t, "none", auth, "an account key says who is coming, it does not vouch for them")
	return sight.values.Get(SessionValueKey)
}

func TestSSHAccountKeyReachesTheProgram(t *testing.T) {
	registered := newClientSigner(t)
	owners := &keyOwners{owners: map[string]string{canonicalKey(registered): "carol-id"}}
	proc := &sessionProcess{seen: make(chan sessionSight, 1)}
	f := startAccountKeyHost(t, proc, owners)

	session, _, _ := openDesktop(t, dialAs(t, f.addr, registered))
	key, present := sessionKey(t, receive(t, proc.seen))
	assert.True(t, present)
	assert.Equal(t, canonicalKey(registered), key, "the key in the form the application stores")
	require.NoError(t, session.Wait())
}

func TestSSHAccountKeyIsFoundAmongOthers(t *testing.T) {
	stranger, registered := newClientSigner(t), newClientSigner(t)
	owners := &keyOwners{owners: map[string]string{canonicalKey(registered): "carol-id"}}
	proc := &sessionProcess{seen: make(chan sessionSight, 1)}
	f := startAccountKeyHost(t, proc, owners)

	// The unregistered key comes first: it must be refused, not let in, so
	// the client goes on to the one that is.
	session, _, _ := openDesktop(t, dialAs(t, f.addr, stranger, registered))
	key, _ := sessionKey(t, receive(t, proc.seen))
	assert.Equal(t, canonicalKey(registered), key)
	owners.mu.Lock()
	assert.Contains(t, owners.asked, canonicalKey(stranger))
	owners.mu.Unlock()
	require.NoError(t, session.Wait())
}

func TestSSHUnregisteredKeyStillMeetsTheLogon(t *testing.T) {
	owners := &keyOwners{owners: map[string]string{}}
	proc := &sessionProcess{seen: make(chan sessionSight, 1)}
	f := startAccountKeyHost(t, proc, owners)

	session, _, _ := openDesktop(t, dialAs(t, f.addr, newClientSigner(t)))
	_, present := sessionKey(t, receive(t, proc.seen))
	assert.False(t, present, "nobody's key: the program asks for the password")
	require.NoError(t, session.Wait())
}

func TestSSHNoKeyAtAllStillMeetsTheLogon(t *testing.T) {
	owners := &keyOwners{owners: map[string]string{}}
	proc := &sessionProcess{seen: make(chan sessionSight, 1)}
	f := startAccountKeyHost(t, proc, owners)

	session, _, _ := openDesktop(t, dialAs(t, f.addr))
	_, present := sessionKey(t, receive(t, proc.seen))
	assert.False(t, present)
	require.NoError(t, session.Wait())
}

func TestSSHFailedKeyLookupCostsOnlyThePassword(t *testing.T) {
	registered := newClientSigner(t)
	owners := &keyOwners{owners: map[string]string{canonicalKey(registered): "carol-id"}, fail: errors.New("no such table")}
	proc := &sessionProcess{seen: make(chan sessionSight, 1)}
	f := startAccountKeyHost(t, proc, owners)

	session, _, _ := openDesktop(t, dialAs(t, f.addr, registered))
	_, present := sessionKey(t, receive(t, proc.seen))
	assert.False(t, present, "a lookup that failed is not a way in")
	require.NoError(t, session.Wait())
}

func TestSSHConfigKeyOwnerNeedsLogon(t *testing.T) {
	cfg := &terminalapi.SSHConfig{Entry: "test:desk", AuthorizedKeys: "keys", KeyOwner: keyOwnerFunction}
	assert.ErrorContains(t, cfg.Validate(), "key_owner is for auth: logon")
	cfg = &terminalapi.SSHConfig{Entry: "test:desk", Auth: terminalapi.SSHAuthLogon, KeyOwner: keyOwnerFunction}
	assert.NoError(t, cfg.Validate())
}
