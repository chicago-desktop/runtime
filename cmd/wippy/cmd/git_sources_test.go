// SPDX-License-Identifier: MPL-2.0

package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bootapi "github.com/wippyai/runtime/api/boot"
	"github.com/wippyai/runtime/api/payload"
	regapi "github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/boot/deps/gitsource/gittest"
	"github.com/wippyai/runtime/boot/deps/hub"
	"github.com/wippyai/runtime/boot/deps/lock"
	syspayload "github.com/wippyai/runtime/system/payload"
	jsonpayload "github.com/wippyai/runtime/system/payload/json"
	"go.uber.org/zap"
)

func newTestTranscoder() payload.Transcoder {
	transcoder := syspayload.NewTranscoder()
	jsonpayload.Register(transcoder)
	return transcoder
}

func TestLockModuleFromResolved(t *testing.T) {
	t.Parallel()
	hubRow := lockModuleFromResolved(hub.ResolvedModule{Org: "acme", Name: "http", Version: "1.0.0", Digest: "sha256:abc"})
	assert.Equal(t, lock.Module{Name: "acme/http", Version: "1.0.0", Hash: "sha256:abc"}, hubRow)

	gitRow := lockModuleFromResolved(hub.ResolvedModule{
		Org: "chicago", Name: "shell", Version: "0.1.3", Source: "git",
		Repository: "github.com/chicago-desktop/shell", Commit: "42c349180724271df4a875998b0c464942da881f",
		Digest: "sha256-tree-v1:deadbeef",
	})
	assert.Equal(t, lock.Module{
		Name: "chicago/shell", Version: "0.1.3",
		Source: "github.com/chicago-desktop/shell", Commit: "42c349180724271df4a875998b0c464942da881f",
		LocalHash: "sha256-tree-v1:deadbeef",
	}, gitRow)
}

func TestExtractRootDependencies_KeepsGitComponentVerbatim(t *testing.T) {
	t.Parallel()
	transcoder := newTestTranscoder()
	entries := []regapi.Entry{
		{ID: regapi.NewID("app.deps", "shell"), Kind: regapi.NamespaceDependency,
			Data: payload.New(map[string]any{"component": "https://github.com/chicago-desktop/shell", "version": ">=0.1.0"})},
		{ID: regapi.NewID("app.deps", "http"), Kind: regapi.NamespaceDependency,
			Data: payload.New(map[string]any{"component": "acme/http", "version": "^1"})},
	}
	deps := extractRootDependencies(entries, transcoder)
	require.Len(t, deps, 2)
	assert.Equal(t, dependencyRequest{Component: "https://github.com/chicago-desktop/shell", Constraint: ">=0.1.0"}, deps[0])
	assert.Equal(t, dependencyRequest{Component: "acme/http", Org: "acme", Module: "http", Constraint: "^1"}, deps[1])
}

// TestGitReplacement_UpdateThenOfflineInstall walks the replacement path of
// the contract: wippy update resolves url#ref to a commit and records it,
// the next process binds the replacement from the lock, and install
// verifies the checkout without git on PATH.
func TestGitReplacement_UpdateThenOfflineInstall(t *testing.T) {
	repo := gittest.New(t, "acme", "widget")
	repo.Commit(t, "0.1.0", map[string]string{"src/_index.json": gittest.Index("acme.widget"), "src/note.txt": "tagged\n"})
	repo.Tag(t, "v0.1.0", false)
	repo.Commit(t, "0.1.1", map[string]string{"src/_index.json": gittest.Index("acme.widget"), "src/note.txt": "branch tip\n"})
	repo.Push(t)

	root := t.TempDir()
	lockPath := filepath.Join(root, lock.DefaultFilename)
	cache := filepath.Join(root, "git-cache")
	cfg := bootapi.NewConfig(
		bootapi.WithSection("boot", map[string]any{"config_dir": root}),
		bootapi.WithSection("workspace", map[string]any{"replacements.acme/widget": repo.Source("main")}),
	)

	// wippy update: resolve the ref, check it out, record it in the lock.
	workLock, err := lock.New(lockPath, lock.WithGitCache(cache), lock.WithWorkspaceConfig(cfg))
	require.NoError(t, err)
	require.NoError(t, resolveGitReplacements(context.Background(), workLock, zap.NewNop()))
	bound, ok := workLock.GetReplacement("acme/widget")
	require.True(t, ok)
	assert.Equal(t, repo.Commits["main"], bound.Commit)
	note, err := os.ReadFile(filepath.Join(bound.To, "src", "note.txt"))
	require.NoError(t, err)
	assert.Equal(t, "branch tip\n", string(note))

	newLock, err := convertResolvedToLock(lockPath, []hub.ResolvedModule{
		{Org: "acme", Name: "widget", Version: "0.1.1"},
	}, ".wippy", "./src")
	require.NoError(t, err)
	require.NoError(t, recordGitReplacements(newLock, workLock))
	require.NoError(t, newLock.Write())

	row, ok := newLock.GetModule("acme/widget")
	require.True(t, ok)
	assert.Equal(t, repo.Source("main"), row.Source)
	assert.Equal(t, repo.Commits["main"], row.Commit)
	assert.Contains(t, row.LocalHash, "sha256-tree-v1:")
	assert.Empty(t, row.Hash)

	// The next process: no git, the lock alone binds the replacement.
	t.Setenv("PATH", t.TempDir())
	reloaded, err := lock.New(lockPath, lock.WithGitCache(cache), lock.WithWorkspaceConfig(cfg))
	require.NoError(t, err)
	require.NoError(t, lock.Validate(reloaded))
	rebound, ok := reloaded.GetReplacement("acme/widget")
	require.True(t, ok)
	assert.Equal(t, bound.To, rebound.To)

	count, err := ensureGitModules(context.Background(), reloaded, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// A tree that no longer matches the lock is refused.
	require.NoError(t, os.WriteFile(filepath.Join(bound.To, "src", "note.txt"), []byte("edited\n"), 0o600))
	_, err = ensureGitModules(context.Background(), reloaded, zap.NewNop())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match lock local_hash")
}

func TestResolveGitReplacements_RefusesSourceWithoutRef(t *testing.T) {
	repo := gittest.New(t, "acme", "widget")
	repo.Commit(t, "0.1.0", map[string]string{"src/_index.json": gittest.Index("acme.widget")})
	repo.Push(t)
	root := t.TempDir()
	cfg := bootapi.NewConfig(
		bootapi.WithSection("boot", map[string]any{"config_dir": root}),
		bootapi.WithSection("workspace", map[string]any{"replacements.acme/widget": repo.Source("")}),
	)
	lockObj, err := lock.New(filepath.Join(root, lock.DefaultFilename), lock.WithGitCache(filepath.Join(root, "cache")), lock.WithWorkspaceConfig(cfg))
	require.NoError(t, err)
	err = resolveGitReplacements(context.Background(), lockObj, zap.NewNop())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "names no ref")
}
