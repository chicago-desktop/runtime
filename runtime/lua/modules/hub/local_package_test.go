// SPDX-License-Identifier: MPL-2.0
package hub

import (
	"github.com/stretchr/testify/require"
	lua "github.com/wippyai/go-lua"
	ctxapi "github.com/wippyai/runtime/api/context"
	secapi "github.com/wippyai/runtime/api/security"
	fsmod "github.com/wippyai/runtime/runtime/lua/modules/fs"
	"github.com/wippyai/runtime/service/fs/directory"
	"github.com/wippyai/wapp"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenLocalPackage(t *testing.T) {
	dir := t.TempDir()
	artifact := buildWappWithResourceForHubTest(t, []wapp.Entry{{ID: wapp.NewID("demo", "hello"), Kind: "registry.entry"}}, wapp.NewID("demo", "files"), map[string]string{"README.txt": "Hello from disk"})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.wapp"), artifact, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.wapp"), []byte("broken"), 0600))
	backend, err := directory.NewFS(dir, 0755, false)
	require.NoError(t, err)
	defer backend.Close()
	l := lua.NewState()
	defer l.Close()
	l.SetContext(hubTestStoreContext(setupContext(), t))
	table, _ := NewModule(Options{}).Build()
	l.SetGlobal("hub", table)
	fsmod.PushFS(l, backend, ".")
	l.SetGlobal("disk", l.Get(-1))
	l.Pop(1)
	require.NoError(t, l.DoString(`
 local pkg, err = hub.open(disk, "demo.wapp")
 assert(pkg, tostring(err))
 assert(#pkg:entries() == 1)
 local files = assert(pkg:fs("demo:files"))
 assert(files:readfile("README.txt") == "Hello from disk")
 assert(pkg:close())
 assert(pkg:close())
 local entries, closed = pkg:entries()
 assert(entries == nil and closed ~= nil)
 local bad, why = hub.open(disk,"bad.wapp")
 assert(bad == nil and why ~= nil)
 local escaped, denied = hub.open(disk,"../demo.wapp")
 assert(escaped == nil and denied ~= nil)
 `))
	l.SetContext(secapi.SetStrictMode(ctxapi.NewRootContext(), true))
	require.NoError(t, l.DoString(`local p,e=hub.open(disk,"demo.wapp"); assert(p==nil and e~=nil)`))
}
