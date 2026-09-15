// SPDX-License-Identifier: MPL-2.0

package lock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/boot"
)

const (
	testCommit = "42c349180724271df4a875998b0c464942da881f"
	testDigest = "sha256-tree-v1:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestLock_GitModuleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, DefaultFilename)
	lockObj, err := New(lockPath)
	require.NoError(t, err)
	lockObj.SetDirectories(Directories{Modules: ".wippy", Src: "./src"})
	lockObj.SetModule(Module{
		Name: "chicago/shell", Version: "0.1.3",
		Source: "github.com/chicago-desktop/shell", Commit: testCommit, LocalHash: testDigest,
	})
	lockObj.SetModule(Module{Name: "acme/http", Version: "1.0.0", Hash: "sha256:" + strings.Repeat("a", 64)})
	require.NoError(t, lockObj.Write())

	data, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "source: github.com/chicago-desktop/shell")
	assert.Contains(t, string(data), "commit: "+testCommit)
	assert.Contains(t, string(data), "local_hash: "+testDigest)
	assert.NotContains(t, string(data), "hash:\n", "a git row has no artifact hash")

	reloaded, err := New(lockPath)
	require.NoError(t, err)
	require.NoError(t, Validate(reloaded))
	mod, ok := reloaded.GetModule("chicago/shell")
	require.True(t, ok)
	assert.True(t, mod.IsGit())
	assert.Equal(t, "github.com/chicago-desktop/shell", mod.Source)
	assert.Equal(t, testCommit, mod.Commit)
	assert.Equal(t, testDigest, mod.LocalHash)
	assert.Empty(t, mod.Hash)

	bySource, ok := reloaded.ModuleForSource("git@github.com:chicago-desktop/shell.git")
	require.True(t, ok, "spellings of one repository find the same row")
	assert.Equal(t, "chicago/shell", bySource.Name)
	_, ok = reloaded.ModuleForSource("github.com/chicago-desktop/weather")
	assert.False(t, ok)
}

func TestValidate_GitModuleNeedsCommitAndLocalHash(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), DefaultFilename)
	newLock := func(mod Module) *Lock {
		lockObj, err := New(lockPath)
		require.NoError(t, err)
		lockObj.SetDirectories(Directories{Modules: ".wippy", Src: "./src"})
		lockObj.SetModule(mod)
		return lockObj
	}

	err := Validate(newLock(Module{Name: "chicago/shell", Version: "0.1.3", Source: "github.com/chicago-desktop/shell", LocalHash: testDigest}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "records no commit")

	err = Validate(newLock(Module{Name: "chicago/shell", Version: "0.1.3", Source: "github.com/chicago-desktop/shell", Commit: testCommit}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "records no local_hash")

	err = Validate(newLock(Module{Name: "chicago/shell", Version: "0.1.3", Source: "github.com/chicago-desktop/shell", Commit: "abc123", LocalHash: testDigest}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a full commit id")

	require.NoError(t, Validate(newLock(Module{Name: "chicago/shell", Version: "0.1.3", Source: "github.com/chicago-desktop/shell", Commit: testCommit, LocalHash: testDigest})))
}

func TestWorkspaceReplacements_GitSourceIsNotAPath(t *testing.T) {
	configDir := t.TempDir()
	cfg := boot.NewConfig(
		boot.WithSection("boot", map[string]any{"config_dir": configDir}),
		boot.WithSection("workspace", map[string]any{
			"replacements.chicago/shell":   "https://github.com/chicago-desktop/shell#v0.1.3",
			"replacements.chicago/weather": "git@github.com:chicago-desktop/weather.git#main",
			"replacements.acme/http":       "../http",
		}),
	)
	replacements, err := WorkspaceReplacements(cfg)
	require.NoError(t, err)
	require.Equal(t, []Replacement{
		{From: "acme/http", To: filepath.Join(configDir, "../http")},
		{From: "chicago/shell", Source: "https://github.com/chicago-desktop/shell#v0.1.3"},
		{From: "chicago/weather", Source: "git@github.com:chicago-desktop/weather.git#main"},
	}, replacements)

	_, err = WorkspaceReplacements(boot.NewConfig(boot.WithSection("workspace", map[string]any{
		"replacements.chicago/shell": "https://github.com/chicago-desktop/shell#",
	})))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty ref")
}

func TestGitReplacement_BindsToLockedCommit(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, DefaultFilename)
	cache := filepath.Join(dir, "cache")
	replacement := Replacement{From: "chicago/shell", Source: "https://github.com/chicago-desktop/shell#main"}

	// Nothing recorded yet: the replacement stays unresolved and the boot
	// refuses it with the command that fixes it.
	pending, err := New(lockPath, WithGitCache(cache), WithWorkspaceReplacements([]Replacement{replacement}))
	require.NoError(t, err)
	pending.SetDirectories(Directories{Modules: ".wippy", Src: "./src"})
	pending.SetModule(Module{Name: "chicago/shell", Version: "0.0.0"})
	got, ok := pending.GetReplacement("chicago/shell")
	require.True(t, ok)
	assert.Empty(t, got.To)
	err = Validate(pending)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "records no commit for it; run wippy update")

	// wippy update resolves the ref and records the commit.
	checkout, ok := pending.BindGitReplacement("chicago/shell", testCommit)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(cache, "github.com", "chicago-desktop", "shell", "checkouts", testCommit), checkout)
	pending.SetModule(Module{Name: "chicago/shell", Version: "0.0.0", Source: replacement.Source, Commit: testCommit, LocalHash: testDigest})
	require.NoError(t, pending.Write())

	// The next process binds the replacement from the lock alone.
	reloaded, err := New(lockPath, WithGitCache(cache), WithWorkspaceReplacements([]Replacement{replacement}))
	require.NoError(t, err)
	got, ok = reloaded.GetReplacement("chicago/shell")
	require.True(t, ok)
	assert.Equal(t, checkout, got.To)
	assert.Equal(t, testCommit, got.Commit)
	require.NoError(t, Validate(reloaded), "the checkout need not exist at validation; install materializes it")

	paths := reloaded.GetModuleLoadPaths()
	require.Len(t, paths, 2)
	assert.Equal(t, checkout, paths[1].SourceRoot)
	assert.Equal(t, replacement.Source, paths[1].Source)
	assert.True(t, paths[1].Replacement)

	// A different repository in the lock does not bind the replacement.
	other, err := New(lockPath, WithGitCache(cache), WithWorkspaceReplacements([]Replacement{
		{From: "chicago/shell", Source: "https://github.com/someone-else/shell#main"},
	}))
	require.NoError(t, err)
	got, _ = other.GetReplacement("chicago/shell")
	assert.Empty(t, got.To)
}

func TestGetModuleLoadPaths_GitModuleLoadsFromCheckout(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "cache")
	lockObj, err := New(filepath.Join(dir, DefaultFilename), WithGitCache(cache))
	require.NoError(t, err)
	lockObj.SetDirectories(Directories{Modules: ".wippy", Src: "./src"})
	lockObj.SetModule(Module{
		Name: "chicago/shell", Version: "0.1.3",
		Source: "github.com/chicago-desktop/shell", Commit: testCommit, LocalHash: testDigest,
	})
	checkout := filepath.Join(cache, "github.com", "chicago-desktop", "shell", "checkouts", testCommit)
	require.NoError(t, os.MkdirAll(filepath.Join(checkout, "src"), 0o755))

	paths := lockObj.GetModuleLoadPaths()
	require.Len(t, paths, 2)
	assert.Equal(t, filepath.Join(checkout, "src"), paths[1].Path)
	assert.Equal(t, checkout, paths[1].SourceRoot)
	assert.Equal(t, "chicago/shell", paths[1].Module)
	assert.Equal(t, testDigest, paths[1].Digest)
	assert.Equal(t, "github.com/chicago-desktop/shell", paths[1].Source)
	assert.False(t, paths[1].Replacement)
}

func TestGitCacheRoot_Default(t *testing.T) {
	dir := t.TempDir()
	lockObj, err := New(filepath.Join(dir, DefaultFilename))
	require.NoError(t, err)
	t.Setenv("WIPPY_GIT_CACHE", "")
	t.Setenv("HOME", "/home/someone")
	assert.Equal(t, filepath.Join("/home/someone", ".wippy", "git"), lockObj.GitCacheRoot())
	t.Setenv("WIPPY_GIT_CACHE", "/var/cache/wippy-git")
	assert.Equal(t, "/var/cache/wippy-git", lockObj.GitCacheRoot())
}
