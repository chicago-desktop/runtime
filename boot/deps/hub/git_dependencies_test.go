// SPDX-License-Identifier: MPL-2.0

package hub

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	regapi "github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/boot/deps/gitsource"
	"github.com/wippyai/runtime/boot/deps/gitsource/gittest"
	"github.com/wippyai/runtime/boot/deps/lock"
	"go.uber.org/zap"
)

// widgetRepo is a module acme/widget tagged v0.1.0, v0.1.3 (annotated) and
// 0.2.0 (no v), with an untagged commit on main after the last tag.
func widgetRepo(t *testing.T) *gittest.Repo {
	t.Helper()
	repo := gittest.New(t, "acme", "widget")
	repo.Commit(t, "0.1.0", map[string]string{"src/_index.json": gittest.Index("acme.widget"), "src/note.txt": "one\n"})
	repo.Tag(t, "v0.1.0", false)
	repo.Commit(t, "0.1.3", map[string]string{"src/_index.json": gittest.Index("acme.widget"), "src/note.txt": "two\n"})
	repo.Tag(t, "v0.1.3", true)
	repo.Commit(t, "0.2.0", map[string]string{"src/_index.json": gittest.Index("acme.widget"), "src/note.txt": "three\n"})
	repo.Tag(t, "0.2.0", false)
	repo.Commit(t, "0.2.0", map[string]string{"src/_index.json": gittest.Index("acme.widget"), "src/note.txt": "unreleased\n"})
	repo.Push(t)
	return repo
}

type gitWorkspace struct {
	lockPath string
	cache    string
	hub      *fakeHub
	hubCalls int
}

func newGitWorkspace(t *testing.T) *gitWorkspace {
	t.Helper()
	root := t.TempDir()
	lockPath := filepath.Join(root, lock.DefaultFilename)
	lockObj, err := lock.New(lockPath)
	require.NoError(t, err)
	lockObj.SetDirectories(lock.Directories{Modules: ".wippy", Src: "src"})
	require.NoError(t, lockObj.Write())
	w := &gitWorkspace{
		lockPath: lockPath,
		cache:    filepath.Join(root, "git-cache"),
	}
	w.hub = &fakeHub{
		listVersions: func(_ context.Context, org, module string) ([]VersionInfo, error) {
			w.hubCalls++
			return nil, fmt.Errorf("hub asked for %s/%s", org, module)
		},
		getManifest: func(_ context.Context, org, module, _ string) (*ModuleManifest, error) {
			w.hubCalls++
			return nil, fmt.Errorf("hub asked for %s/%s", org, module)
		},
	}
	return w
}

func (w *gitWorkspace) handler(t *testing.T, refresh bool) *DependencyHandler {
	t.Helper()
	handler, err := NewDependencyHandler(DependencyHandlerOptions{
		Hub:               w.hub,
		Logger:            zap.NewNop(),
		LockPath:          w.lockPath,
		GitCache:          w.cache,
		RefreshGitSources: refresh,
	})
	require.NoError(t, err)
	return handler
}

// record writes the resolved graph into the lock the way wippy update does.
func (w *gitWorkspace) record(t *testing.T, modules []ResolvedModule) {
	t.Helper()
	lockObj, err := lock.New(w.lockPath)
	require.NoError(t, err)
	rows := make([]lock.Module, 0, len(modules))
	for _, mod := range modules {
		row := lock.Module{Name: mod.Org + "/" + mod.Name, Version: mod.Version, Hash: mod.Digest}
		if mod.Source == moduleSourceGit {
			row = lock.Module{Name: row.Name, Version: mod.Version, Source: mod.Repository, Commit: mod.Commit, LocalHash: mod.Digest}
		}
		rows = append(rows, row)
	}
	lockObj.ReplaceModules(rows)
	require.NoError(t, lockObj.Write())
}

func offlineContext() context.Context {
	return regapi.WithDependencyAccess(newTestContext(), regapi.DependencyAccessVerifiedOffline)
}

func TestGitDependency_PicksHighestTagInRange(t *testing.T) {
	repo := widgetRepo(t)
	w := newGitWorkspace(t)
	handler := w.handler(t, true)

	modules, err := handler.ResolveWorkspaceDependencies(newTestContext(), []DependencyDefinition{
		{Component: repo.Source(""), Version: ">=0.1.0 <0.2.0"},
	})
	require.NoError(t, err)
	require.Len(t, modules, 1)
	mod := modules[0]
	assert.Equal(t, "acme/widget", mod.Org+"/"+mod.Name)
	assert.Equal(t, "0.1.3", mod.Version, "the annotated v0.1.3 tag, recorded without the v")
	assert.Equal(t, moduleSourceGit, mod.Source)
	assert.Equal(t, repo.Source(""), mod.Repository)
	assert.Equal(t, repo.Commits["v0.1.3"], mod.Commit, "the peeled commit, not the tag object")
	assert.True(t, strings.HasPrefix(mod.Digest, "sha256-tree-v1:"), mod.Digest)
	assert.Zero(t, w.hubCalls, "a git module never asks the Hub")

	checkout := gitsource.New(w.cache).CheckoutDir(mustParse(t, repo.Source("")), mod.Commit)
	note, err := os.ReadFile(filepath.Join(checkout, "src", "note.txt"))
	require.NoError(t, err)
	assert.Equal(t, "two\n", string(note))
}

func TestGitDependency_WildcardTakesHighestTagNotBranch(t *testing.T) {
	repo := widgetRepo(t)
	w := newGitWorkspace(t)

	modules, err := w.handler(t, true).ResolveWorkspaceDependencies(newTestContext(), []DependencyDefinition{
		{Component: repo.Source(""), Version: "*"},
	})
	require.NoError(t, err)
	require.Len(t, modules, 1)
	assert.Equal(t, "0.2.0", modules[0].Version, "a tag without the v")
	assert.Equal(t, repo.Commits["0.2.0"], modules[0].Commit, "not the unreleased tip of main")
}

func TestGitDependency_RefusesRangeWithNoMatchingTag(t *testing.T) {
	repo := widgetRepo(t)
	w := newGitWorkspace(t)

	_, err := w.handler(t, true).ResolveWorkspaceDependencies(newTestContext(), []DependencyDefinition{
		{Component: repo.Source(""), Version: ">=1.0.0"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no tag satisfies")
	assert.Contains(t, err.Error(), "v0.1.3")
	assert.Contains(t, err.Error(), "0.2.0")
}

func TestGitDependency_RefusesRepositoryWithoutTags(t *testing.T) {
	repo := gittest.New(t, "acme", "untagged")
	repo.Commit(t, "0.1.0", map[string]string{"src/_index.json": gittest.Index("acme.untagged")})
	repo.Push(t)
	w := newGitWorkspace(t)

	_, err := w.handler(t, true).ResolveWorkspaceDependencies(newTestContext(), []DependencyDefinition{
		{Component: repo.Source(""), Version: "*"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a branch is not a version")
}

func TestGitDependency_Transitive(t *testing.T) {
	widget := widgetRepo(t)
	shell := gittest.New(t, "acme", "shell")
	shell.Commit(t, "1.0.0", map[string]string{
		"src/_index.json": gittest.Index("acme.shell", gittest.Dependency("widget", widget.Source(""), "^0.1.0")),
	})
	shell.Tag(t, "v1.0.0", false)
	shell.Push(t)
	w := newGitWorkspace(t)

	modules, err := w.handler(t, true).ResolveWorkspaceDependencies(newTestContext(), []DependencyDefinition{
		{Component: shell.Source(""), Version: "*"},
	})
	require.NoError(t, err)
	require.Len(t, modules, 2)
	byName := make(map[string]ResolvedModule, 2)
	for _, mod := range modules {
		byName[mod.Org+"/"+mod.Name] = mod
	}
	assert.Equal(t, "1.0.0", byName["acme/shell"].Version)
	assert.Equal(t, shell.Commits["v1.0.0"], byName["acme/shell"].Commit)
	assert.Equal(t, "0.1.3", byName["acme/widget"].Version, "the transitive range ^0.1 picks the highest 0.1.x tag")
	assert.Equal(t, widget.Commits["v0.1.3"], byName["acme/widget"].Commit)
	assert.Equal(t, moduleSourceGit, byName["acme/widget"].Source)
}

func TestGitDependency_ConflictBetweenTwoSourcesForOneModule(t *testing.T) {
	first := widgetRepo(t)
	second := gittest.New(t, "acme", "widget")
	second.Commit(t, "0.5.0", map[string]string{"src/_index.json": gittest.Index("acme.widget")})
	second.Tag(t, "v0.5.0", false)
	second.Push(t)
	w := newGitWorkspace(t)

	_, err := w.handler(t, true).ResolveWorkspaceDependencies(newTestContext(), []DependencyDefinition{
		{Component: first.Source(""), Version: "*"},
		{Component: second.Source(""), Version: "*"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "module acme/widget is provided by two git sources")
	assert.Contains(t, err.Error(), first.Bare)
	assert.Contains(t, err.Error(), second.Bare)
}

func TestGitDependency_SecondResolveIsOfflineFromLock(t *testing.T) {
	repo := widgetRepo(t)
	w := newGitWorkspace(t)

	online, err := w.handler(t, true).ResolveWorkspaceDependencies(newTestContext(), []DependencyDefinition{
		{Component: repo.Source(""), Version: "^0.1.0"},
	})
	require.NoError(t, err)
	w.record(t, online)

	// No git on PATH: the lock names the commit, the cache holds the checkout.
	t.Setenv("PATH", t.TempDir())
	handler := w.handler(t, false)
	offline, err := handler.ResolveWorkspaceDependencies(offlineContext(), []DependencyDefinition{
		{Component: repo.Source(""), Version: "^0.1.0"},
	})
	require.NoError(t, err)
	require.Len(t, offline, 1)
	assert.Equal(t, online[0].Version, offline[0].Version)
	assert.Equal(t, online[0].Commit, offline[0].Commit)
	assert.Equal(t, online[0].Digest, offline[0].Digest)
	assert.Equal(t, moduleSourceGit, offline[0].Source)

	path, err := handler.ensureModuleAvailable(offlineContext(), offline[0])
	require.NoError(t, err)
	assert.Equal(t, gitsource.New(w.cache).CheckoutDir(mustParse(t, repo.Source("")), online[0].Commit), path)
}

func TestGitDependency_OfflineWithoutLockIsRefused(t *testing.T) {
	repo := widgetRepo(t)
	w := newGitWorkspace(t)

	_, err := w.handler(t, false).ResolveWorkspaceDependencies(offlineContext(), []DependencyDefinition{
		{Component: repo.Source(""), Version: "^0.1.0"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "offline")
}

func TestGitDependency_ChangedTreeIsRefused(t *testing.T) {
	repo := widgetRepo(t)
	w := newGitWorkspace(t)

	online, err := w.handler(t, true).ResolveWorkspaceDependencies(newTestContext(), []DependencyDefinition{
		{Component: repo.Source(""), Version: "^0.1.0"},
	})
	require.NoError(t, err)
	w.record(t, online)

	checkout := gitsource.New(w.cache).CheckoutDir(mustParse(t, repo.Source("")), online[0].Commit)
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "src", "note.txt"), []byte("edited in the cache\n"), 0o600))

	_, err = w.handler(t, false).ensureModuleAvailable(offlineContext(), online[0])
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tree digest mismatch")
}

func TestGitDependency_MissingGitNamesTheSource(t *testing.T) {
	repo := widgetRepo(t)
	w := newGitWorkspace(t)
	t.Setenv("PATH", t.TempDir())

	_, err := w.handler(t, true).ResolveWorkspaceDependencies(newTestContext(), []DependencyDefinition{
		{Component: repo.Source(""), Version: "*"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "git binary not found")
	assert.Contains(t, err.Error(), repo.Bare)
}

func TestGitDependency_ReplacementPinsBranch(t *testing.T) {
	repo := widgetRepo(t)
	w := newGitWorkspace(t)
	src := mustParse(t, repo.Source("main"))
	cache := gitsource.New(w.cache)
	commit, err := cache.ResolveRef(context.Background(), src, "main")
	require.NoError(t, err)
	checkout, err := cache.Checkout(context.Background(), src, commit)
	require.NoError(t, err)

	handler, err := NewDependencyHandler(DependencyHandlerOptions{
		Hub:      w.hub,
		Logger:   zap.NewNop(),
		LockPath: w.lockPath,
		GitCache: w.cache,
		WorkspaceReplacements: []lock.Replacement{
			{From: "acme/widget", To: checkout, Source: repo.Source("main"), Commit: commit},
		},
	})
	require.NoError(t, err)

	modules, err := handler.ResolveWorkspaceDependencies(newTestContext(), []DependencyDefinition{
		{Component: "acme/widget", Version: "*"},
	})
	require.NoError(t, err)
	require.Len(t, modules, 1)
	assert.Equal(t, "0.2.0", modules[0].Version, "the version the branch tip declares")
	assert.NotEqual(t, moduleSourceGit, modules[0].Source, "a replacement is a directory, whatever its origin")

	path, err := handler.ensureModuleAvailable(newTestContext(), modules[0])
	require.NoError(t, err)
	assert.Equal(t, checkout, path)
	note, err := os.ReadFile(filepath.Join(path, "src", "note.txt"))
	require.NoError(t, err)
	assert.Equal(t, "unreleased\n", string(note), "the branch tip, not a tag")
}

func TestSelectGitVersion(t *testing.T) {
	t.Parallel()
	tags := gitSemverTags([]gitsource.Tag{
		{Name: "v0.1.0", Commit: "a"}, {Name: "v0.1.3", Commit: "b"}, {Name: "0.2.0", Commit: "c"},
		{Name: "v0.3.0-rc.1", Commit: "d"}, {Name: "not-a-version", Commit: "e"},
	})
	cases := map[string]string{"*": "0.2.0", "": "0.2.0", "^0.1.0": "0.1.3", ">=0.1.0 <0.1.3": "0.1.0", "=0.1.3": "0.1.3", "v0.1.0": "0.1.0", ">=0.3.0-rc.1": "0.3.0-rc.1"}
	for constraint, want := range cases {
		got, err := selectGitVersion(tags, constraint)
		require.NoError(t, err, constraint)
		assert.Equal(t, want, got, constraint)
	}
	_, err := selectGitVersion(tags, ">=1.0.0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "v0.1.0, v0.1.3, v0.3.0-rc.1")
}

func mustParse(t *testing.T, value string) gitsource.Source {
	t.Helper()
	src, err := gitsource.Parse(value)
	require.NoError(t, err)
	return src
}

// TestGitDependency_CanonicalRootKeepsGitIdentity is the boot: the loader has
// already rewritten the component to the module name, so the handler sees
// acme/widget, not the repository. The lock's git row must still make the
// module a git module - source, commit, tree digest - or identity completion
// refuses it as a Hub artifact with the wrong digest.
func TestGitDependency_CanonicalRootKeepsGitIdentity(t *testing.T) {
	repo := widgetRepo(t)
	w := newGitWorkspace(t)

	online, err := w.handler(t, true).ResolveWorkspaceDependencies(newTestContext(), []DependencyDefinition{
		{Component: repo.Source(""), Version: "^0.1.0"},
	})
	require.NoError(t, err)
	w.record(t, online)

	for name, ctx := range map[string]context.Context{"online": newTestContext(), "offline": offlineContext()} {
		t.Run(name, func(t *testing.T) {
			handler := w.handler(t, false)
			resolved, err := handler.resolveEffectiveModules(ctx, []DependencyDefinition{
				{Component: "acme/widget", Version: "^0.1.0"},
			}, map[string]string{"acme/widget": "0.1.3"}, nil)
			require.NoError(t, err)
			require.Len(t, resolved, 1)
			assert.Equal(t, moduleSourceGit, resolved[0].Source)
			assert.Equal(t, repo.Source(""), resolved[0].Repository)
			assert.Equal(t, repo.Commits["v0.1.3"], resolved[0].Commit)
			assert.Equal(t, online[0].Digest, resolved[0].Digest)
			assert.Zero(t, w.hubCalls, "the lock names the repository; the Hub is never asked")
		})
	}
}

// TestGitDependency_UpdateFollowsMovedTag: a tag re-pointed upstream is what
// wippy update exists to pick up; the tree the lock pinned is being replaced,
// not a bound the new tree must match.
func TestGitDependency_UpdateFollowsMovedTag(t *testing.T) {
	repo := widgetRepo(t)
	w := newGitWorkspace(t)

	first, err := w.handler(t, true).ResolveWorkspaceDependencies(newTestContext(), []DependencyDefinition{
		{Component: repo.Source(""), Version: "*"},
	})
	require.NoError(t, err)
	require.Equal(t, repo.Commits["0.2.0"], first[0].Commit)
	w.record(t, first)

	moved := repo.Commit(t, "0.2.0", map[string]string{"src/_index.json": gittest.Index("acme.widget"), "src/note.txt": "re-tagged\n"})
	repo.Git(t, repo.Work, "tag", "-f", "0.2.0")
	repo.Git(t, repo.Work, "push", "--quiet", "--force", repo.Bare, "main", "--tags")

	second, err := w.handler(t, true).ResolveWorkspaceDependencies(newTestContext(), []DependencyDefinition{
		{Component: repo.Source(""), Version: "*"},
	})
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, "0.2.0", second[0].Version)
	assert.Equal(t, moved, second[0].Commit)
	assert.NotEqual(t, first[0].Digest, second[0].Digest)

	// Not refreshing (the boot, a plain run): the lock's commit stands.
	pinned, err := w.handler(t, false).ResolveWorkspaceDependencies(offlineContext(), []DependencyDefinition{
		{Component: repo.Source(""), Version: "*"},
	})
	require.NoError(t, err)
	assert.Equal(t, first[0].Commit, pinned[0].Commit)
}

// writeReplacementTree writes a module tree standing in for a repository:
// its own version, and the repository it stands in for when named.
func writeReplacementTree(t *testing.T, dir, name, version, repository string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	parts := strings.SplitN(name, "/", 2)
	manifest := "organization: " + parts[0] + "\nmodule: " + parts[1] + "\nversion: " + version + "\n"
	if repository != "" {
		manifest += "repository: " + repository + "\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "wippy.yaml"), []byte(manifest), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "_index.json"), []byte(gittest.Index("acme.widget")), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "note.txt"), []byte("from the working copy\n"), 0o600))
}

func (w *gitWorkspace) handlerWithReplacements(t *testing.T, refresh bool, replacements ...lock.Replacement) *DependencyHandler {
	t.Helper()
	handler, err := NewDependencyHandler(DependencyHandlerOptions{
		Hub:                   w.hub,
		Logger:                zap.NewNop(),
		LockPath:              w.lockPath,
		GitCache:              w.cache,
		RefreshGitSources:     refresh,
		WorkspaceReplacements: replacements,
	})
	require.NoError(t, err)
	return handler
}

// TestGitDependency_ReplacedGitDeclaredModule is the shell's setup: the
// declaration names the repository, a workspace replacement supplies the
// directory. update records the tag's evidence in the row; the boot binds
// the declaration from it offline and loads the directory.
func TestGitDependency_ReplacedGitDeclaredModule(t *testing.T) {
	repo := widgetRepo(t)
	w := newGitWorkspace(t)
	replacement := filepath.Join(t.TempDir(), "widget-copy")
	writeReplacementTree(t, replacement, "acme/widget", "0.1.9", "")
	replaced := lock.Replacement{From: "acme/widget", To: replacement}
	roots := []DependencyDefinition{{Component: repo.Source(""), Version: "^0.1.0"}}

	// wippy update: the directory's version, the tag's evidence.
	resolved, err := w.handlerWithReplacements(t, true, replaced).ResolveWorkspaceDependencies(newTestContext(), roots)
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	mod := resolved[0]
	assert.Equal(t, "0.1.9", mod.Version, "the version the directory declares, not a tag")
	assert.NotEqual(t, moduleSourceGit, mod.Source, "the module's identity stays the directory's")
	assert.Equal(t, repo.Source(""), mod.Repository)
	assert.Equal(t, repo.Commits["v0.1.3"], mod.Commit, "the tag the declaration's range selected")
	tagDigest, _, err := digestReplacementTree(gitsource.New(w.cache).CheckoutDir(mustParse(t, repo.Source("")), mod.Commit))
	require.NoError(t, err)
	assert.Equal(t, tagDigest, mod.Digest, "the tag's tree digest, not the directory's")

	lockObj, err := lock.New(w.lockPath)
	require.NoError(t, err)
	lockObj.ReplaceModules([]lock.Module{{Name: "acme/widget", Version: mod.Version, Source: mod.Repository, Commit: mod.Commit, LocalHash: mod.Digest}})
	require.NoError(t, lockObj.Write())

	// The boot: no git on PATH, the declaration binds from the row, the
	// module loads from the directory.
	t.Setenv("PATH", t.TempDir())
	handler := w.handlerWithReplacements(t, false, replaced)
	booted, err := handler.resolveEffectiveModules(offlineContext(), roots, map[string]string{"acme/widget": "0.1.9"}, nil)
	require.NoError(t, err)
	require.Len(t, booted, 1)
	assert.Equal(t, moduleSourceReplacementTreeV1, booted[0].Source)
	assert.Equal(t, "0.1.9", booted[0].Version)
	path, err := handler.ensureModuleAvailable(offlineContext(), booted[0])
	require.NoError(t, err)
	assert.Equal(t, replacement, path)
	assert.Zero(t, w.hubCalls)
}

// TestGitDependency_ReplacementManifestNamesTheRepository: with no row in
// the lock, a directory whose wippy.yaml names the repository binds the
// declaration by itself.
func TestGitDependency_ReplacementManifestNamesTheRepository(t *testing.T) {
	repo := widgetRepo(t)
	w := newGitWorkspace(t)
	replacement := filepath.Join(t.TempDir(), "widget-copy")
	writeReplacementTree(t, replacement, "acme/widget", "0.1.9", "git+"+repo.Bare)
	replaced := lock.Replacement{From: "acme/widget", To: replacement}
	roots := []DependencyDefinition{{Component: repo.Source(""), Version: "^0.1.0"}}

	t.Setenv("PATH", t.TempDir())
	handler := w.handlerWithReplacements(t, false, replaced)
	booted, err := handler.resolveEffectiveModules(offlineContext(), roots, map[string]string{"acme/widget": "0.1.9"}, nil)
	require.NoError(t, err)
	require.Len(t, booted, 1)
	assert.Equal(t, moduleSourceReplacementTreeV1, booted[0].Source)
	path, err := handler.ensureModuleAvailable(offlineContext(), booted[0])
	require.NoError(t, err)
	assert.Equal(t, replacement, path)

	// A directory that names no repository cannot bind the declaration.
	writeReplacementTree(t, replacement, "acme/widget", "0.1.9", "")
	_, err = w.handlerWithReplacements(t, false, replaced).resolveEffectiveModules(offlineContext(), roots, map[string]string{"acme/widget": "0.1.9"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "offline")
}
