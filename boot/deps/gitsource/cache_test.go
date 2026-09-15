// SPDX-License-Identifier: MPL-2.0

package gitsource

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixture is a bare repository with a few tags, made with git init in a
// temporary directory: no network anywhere in these tests.
type fixture struct {
	bare    string
	work    string
	commits map[string]string // tag or branch -> commit
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	f := &fixture{
		bare:    filepath.Join(root, "origin.git"),
		work:    filepath.Join(root, "work"),
		commits: make(map[string]string),
	}
	run(t, root, "init", "--quiet", "--bare", "-b", "main", f.bare)
	run(t, root, "init", "--quiet", "-b", "main", f.work)
	f.commit(t, "0.1.0", "one")
	run(t, f.work, "tag", "v0.1.0")
	f.commits["v0.1.0"] = run(t, f.work, "rev-parse", "HEAD")
	f.commit(t, "0.1.3", "two")
	run(t, f.work, "tag", "-a", "v0.1.3", "-m", "release 0.1.3")
	f.commits["v0.1.3"] = run(t, f.work, "rev-parse", "HEAD")
	f.commit(t, "0.2.0", "three")
	run(t, f.work, "tag", "0.2.0")
	f.commits["0.2.0"] = run(t, f.work, "rev-parse", "HEAD")
	run(t, f.work, "tag", "not-a-version")
	f.commit(t, "0.2.0", "main tip")
	f.commits["main"] = run(t, f.work, "rev-parse", "HEAD")
	run(t, f.work, "push", "--quiet", f.bare, "main", "--tags")
	return f
}

func (f *fixture) commit(t *testing.T, version, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.work, "wippy.yaml"),
		[]byte("organization: acme\nmodule: widget\nversion: "+version+"\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(f.work, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(f.work, "src", "note.txt"), []byte(content+"\n"), 0o600))
	run(t, f.work, "add", ".")
	run(t, f.work, "commit", "--quiet", "-m", content)
}

func (f *fixture) source(t *testing.T, ref string) Source {
	t.Helper()
	value := f.bare
	if ref != "" {
		value += "#" + ref
	}
	src, err := Parse(value)
	require.NoError(t, err)
	return src
}

func TestCache_TagsListsPeeledCommits(t *testing.T) {
	f := newFixture(t)
	cache := New(t.TempDir())

	tags, err := cache.Tags(context.Background(), f.source(t, ""))
	require.NoError(t, err)
	got := make(map[string]string, len(tags))
	for _, tag := range tags {
		got[tag.Name] = tag.Commit
	}
	assert.Equal(t, map[string]string{
		"v0.1.0":        f.commits["v0.1.0"],
		"v0.1.3":        f.commits["v0.1.3"], // annotated: the peeled commit, not the tag object
		"0.2.0":         f.commits["0.2.0"],
		"not-a-version": f.commits["0.2.0"],
	}, got)
}

func TestCache_ResolveRef(t *testing.T) {
	f := newFixture(t)
	cache := New(t.TempDir())
	ctx := context.Background()
	src := f.source(t, "")

	tag, err := cache.ResolveRef(ctx, src, "v0.1.3")
	require.NoError(t, err)
	assert.Equal(t, f.commits["v0.1.3"], tag)

	branch, err := cache.ResolveRef(ctx, src, "main")
	require.NoError(t, err)
	assert.Equal(t, f.commits["main"], branch)

	commit, err := cache.ResolveRef(ctx, src, strings.ToUpper(f.commits["v0.1.0"]))
	require.NoError(t, err)
	assert.Equal(t, f.commits["v0.1.0"], commit)

	_, err = cache.ResolveRef(ctx, src, "no-such-ref")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-such-ref")
}

func TestCache_CheckoutWritesTreeWithoutDotGit(t *testing.T) {
	f := newFixture(t)
	root := t.TempDir()
	cache := New(root)
	src := f.source(t, "")

	dir, err := cache.Checkout(context.Background(), src, f.commits["v0.1.3"])
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "local", filepath.ToSlash(strings.TrimSuffix(strings.TrimPrefix(f.bare, "/"), ".git")), "checkouts", f.commits["v0.1.3"]), dir)

	note, err := os.ReadFile(filepath.Join(dir, "src", "note.txt"))
	require.NoError(t, err)
	assert.Equal(t, "two\n", string(note))
	_, err = os.Stat(filepath.Join(dir, ".git"))
	assert.True(t, os.IsNotExist(err), "a checkout must not carry a .git")
	_, err = os.Stat(cache.RepoDir(src))
	require.NoError(t, err, "the bare clone lives next to the checkouts")
	entries, err := os.ReadDir(filepath.Dir(dir))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "staging directories are cleaned up")
}

func TestCache_SecondCheckoutNeedsNoGit(t *testing.T) {
	f := newFixture(t)
	cache := New(t.TempDir())
	src := f.source(t, "")
	ctx := context.Background()

	first, err := cache.Checkout(ctx, src, f.commits["0.2.0"])
	require.NoError(t, err)

	// With git gone from PATH the checkout is still answered from the cache.
	t.Setenv("PATH", t.TempDir())
	second, err := cache.Checkout(ctx, src, f.commits["0.2.0"])
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.True(t, cache.HasCheckout(src, f.commits["0.2.0"]))

	// A commit that is not checked out needs git, and the error names it.
	_, err = cache.Checkout(ctx, src, f.commits["v0.1.0"])
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrGitNotFound), err.Error())
}

func TestCache_MissingGitNamesTheSource(t *testing.T) {
	f := newFixture(t)
	cache := New(t.TempDir())
	src := f.source(t, "v0.1.3")
	t.Setenv("PATH", t.TempDir())

	_, err := cache.Tags(context.Background(), src)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrGitNotFound))
	assert.Contains(t, err.Error(), src.Raw)
}

func TestCache_CheckoutRefusesNonCommit(t *testing.T) {
	cache := New(t.TempDir())
	src, err := Parse("github.com/acme/widget")
	require.NoError(t, err)
	_, err = cache.Checkout(context.Background(), src, "v0.1.3")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a commit id")
}

func TestCache_FetchesCommitIntoExistingClone(t *testing.T) {
	f := newFixture(t)
	cache := New(t.TempDir())
	src := f.source(t, "")
	ctx := context.Background()

	_, err := cache.Checkout(ctx, src, f.commits["v0.1.0"])
	require.NoError(t, err)

	// A new commit appears upstream after the clone; it is fetched on demand.
	f.commit(t, "0.3.0", "four")
	run(t, f.work, "tag", "v0.3.0")
	run(t, f.work, "push", "--quiet", f.bare, "main", "--tags")
	latest := run(t, f.work, "rev-parse", "HEAD")

	dir, err := cache.Checkout(ctx, src, latest)
	require.NoError(t, err)
	note, err := os.ReadFile(filepath.Join(dir, "src", "note.txt"))
	require.NoError(t, err)
	assert.Equal(t, "four\n", string(note))
}

func TestDefaultRoot(t *testing.T) {
	t.Setenv(CacheEnv, "/tmp/elsewhere")
	assert.Equal(t, "/tmp/elsewhere", DefaultRoot("fallback"))
	t.Setenv(CacheEnv, "")
	t.Setenv("HOME", "/home/someone")
	assert.Equal(t, filepath.Join("/home/someone", ".wippy", "git"), DefaultRoot("fallback"))
}
