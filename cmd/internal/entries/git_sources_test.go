// SPDX-License-Identifier: MPL-2.0

package entries

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/payload"
	regapi "github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/boot/deps/gitsource/gittest"
	"github.com/wippyai/runtime/boot/deps/hub"
	"github.com/wippyai/runtime/boot/deps/lock"
	syspayload "github.com/wippyai/runtime/system/payload"
	jsonpayload "github.com/wippyai/runtime/system/payload/json"
	"go.uber.org/zap"
)

func TestEnsureGitModuleCheckout_OfflineWhenCachedAndRefusesChangedTree(t *testing.T) {
	repo := gittest.New(t, "acme", "widget")
	repo.Commit(t, "0.1.0", map[string]string{"src/_index.json": gittest.Index("acme.widget"), "src/note.txt": "one\n"})
	repo.Tag(t, "v0.1.0", false)
	repo.Push(t)

	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	lockObj, err := lock.New(filepath.Join(root, lock.DefaultFilename), lock.WithGitCache(cache))
	require.NoError(t, err)
	lockObj.SetDirectories(lock.Directories{Modules: ".wippy", Src: "./src"})

	// The first checkout needs git; its tree digest becomes local_hash.
	mod := lock.Module{Name: "acme/widget", Version: "0.1.0", Source: repo.Source(""), Commit: repo.Commits["v0.1.0"]}
	dir, ok := lockObj.GitCheckoutDir(mod)
	require.True(t, ok)
	err = EnsureGitModuleCheckout(context.Background(), lockObj, mod, zap.NewNop())
	require.Error(t, err, "a row without local_hash cannot be verified")
	digest, _, err := hub.ReplacementTreeIdentity(dir)
	require.NoError(t, err)
	mod.LocalHash = digest

	// Second time: no git on PATH, still fine.
	t.Setenv("PATH", t.TempDir())
	require.NoError(t, EnsureGitModuleCheckout(context.Background(), lockObj, mod, zap.NewNop()))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "note.txt"), []byte("changed\n"), 0o600))
	err = EnsureGitModuleCheckout(context.Background(), lockObj, mod, zap.NewNop())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match lock local_hash")
}

func TestCanonicalizeGitComponents(t *testing.T) {
	transcoder := syspayload.NewTranscoder()
	jsonpayload.Register(transcoder)
	entries := []regapi.Entry{
		{ID: regapi.NewID("app.deps", "shell"), Kind: regapi.NamespaceDependency,
			Data: payload.New(map[string]any{"component": "git@github.com:chicago-desktop/shell.git", "version": ">=0.1.0"})},
		{ID: regapi.NewID("app.deps", "unknown"), Kind: regapi.NamespaceDependency,
			Data: payload.New(map[string]any{"component": "github.com/chicago-desktop/weather", "version": "*"})},
		{ID: regapi.NewID("app.deps", "http"), Kind: regapi.NamespaceDependency,
			Data: payload.New(map[string]any{"component": "acme/http", "version": "^1"})},
		{ID: regapi.NewID("app", "svc"), Kind: "service", Data: payload.New(map[string]any{"component": "irrelevant"})},
	}
	require.NoError(t, canonicalizeGitComponents(entries, map[string]string{
		"https://github.com/chicago-desktop/shell": "chicago/shell",
	}, transcoder))

	component := func(i int) string {
		var data map[string]any
		require.NoError(t, transcoder.Unmarshal(entries[i].Data, &data))
		c, _ := data["component"].(string)
		return c
	}
	assert.Equal(t, "chicago/shell", component(0), "another spelling of the same repository is canonicalized")
	assert.Equal(t, "github.com/chicago-desktop/weather", component(1), "a repository the lock does not know stays as written")
	assert.Equal(t, "acme/http", component(2))
	assert.Equal(t, "irrelevant", component(3), "only ns.dependency entries are touched")
}
