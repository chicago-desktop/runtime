// SPDX-License-Identifier: MPL-2.0

package gitsource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// CacheEnv names the environment variable that overrides the cache root.
const CacheEnv = "WIPPY_GIT_CACHE"

// ErrGitNotFound reports that no git binary is on PATH.
var ErrGitNotFound = errors.New("git binary not found on PATH")

// Tag is one tag of a repository with the commit it points at (peeled for an
// annotated tag).
type Tag struct {
	Name   string
	Commit string
}

// Cache holds one bare clone per repository and one checkout per commit:
//
//	<root>/<host>/<path>/repo.git
//	<root>/<host>/<path>/checkouts/<commit>/
//
// A checkout is written once and never modified; a commit that is already
// checked out needs neither git nor the network.
type Cache struct {
	root string
}

// DefaultRoot returns the cache root: WIPPY_GIT_CACHE when set, otherwise
// $HOME/.wippy/git. With no home directory it returns fallback, so a caller
// without one names its own location instead of guessing.
func DefaultRoot(fallback string) string {
	if root := strings.TrimSpace(os.Getenv(CacheEnv)); root != "" {
		return root
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".wippy", "git")
	}
	return fallback
}

// New returns a cache rooted at root.
func New(root string) *Cache {
	return &Cache{root: root}
}

// Root returns the cache root.
func (c *Cache) Root() string {
	return c.root
}

func (c *Cache) repositoryDir(src Source) string {
	return filepath.Join(c.root, filepath.FromSlash(src.Key))
}

// RepoDir returns the bare clone's directory for src, whether or not it exists.
func (c *Cache) RepoDir(src Source) string {
	return filepath.Join(c.repositoryDir(src), "repo.git")
}

// CheckoutDir returns the checkout directory for commit, whether or not it exists.
func (c *Cache) CheckoutDir(src Source, commit string) string {
	return filepath.Join(c.repositoryDir(src), "checkouts", strings.ToLower(strings.TrimSpace(commit)))
}

// HasCheckout reports whether commit is already materialized.
func (c *Cache) HasCheckout(src Source, commit string) bool {
	info, err := os.Stat(c.CheckoutDir(src, commit))
	return err == nil && info.IsDir()
}

// Tags lists the repository's tags with the commits they point at. This is a
// network call (git ls-remote).
func (c *Cache) Tags(ctx context.Context, src Source) ([]Tag, error) {
	out, err := c.git(ctx, src, "", "ls-remote", "--tags", src.URL)
	if err != nil {
		return nil, err
	}
	commits := make(map[string]string)
	peeled := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[1], "refs/tags/") {
			continue
		}
		name := strings.TrimPrefix(fields[1], "refs/tags/")
		if strings.HasSuffix(name, "^{}") {
			name = strings.TrimSuffix(name, "^{}")
			commits[name] = fields[0]
			peeled[name] = true
			continue
		}
		if !peeled[name] {
			commits[name] = fields[0]
		}
	}
	tags := make([]Tag, 0, len(commits))
	for name, commit := range commits {
		tags = append(tags, Tag{Name: name, Commit: commit})
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].Name < tags[j].Name })
	return tags, nil
}

// ResolveRef resolves a tag, branch or commit spelling to a commit id. A full
// commit id is returned as is; anything else is asked of the remote, tags
// before branches. This is a network call for a non-commit ref.
func (c *Cache) ResolveRef(ctx context.Context, src Source, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("git source %s: ref is empty", src.Raw)
	}
	if IsCommit(ref) {
		return strings.ToLower(ref), nil
	}
	out, err := c.git(ctx, src, "", "ls-remote", src.URL, "refs/tags/"+ref, "refs/tags/"+ref+"^{}", "refs/heads/"+ref)
	if err != nil {
		return "", err
	}
	found := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			found[fields[1]] = fields[0]
		}
	}
	for _, candidate := range []string{"refs/tags/" + ref + "^{}", "refs/tags/" + ref, "refs/heads/" + ref} {
		if commit, ok := found[candidate]; ok {
			return commit, nil
		}
	}
	return "", fmt.Errorf("git source %s: ref %q is neither a tag nor a branch of %s", src.Raw, ref, src.URL)
}

// Checkout materializes commit and returns its directory. An existing
// checkout is returned without running git. Otherwise the bare clone is
// created or the commit fetched into it, and the tree is written through
// `git --work-tree=<dir> checkout <commit> -- .`, so the checkout holds no
// .git of its own.
func (c *Cache) Checkout(ctx context.Context, src Source, commit string) (string, error) {
	commit = strings.ToLower(strings.TrimSpace(commit))
	if !IsCommit(commit) {
		return "", fmt.Errorf("git source %s: %q is not a commit id", src.Raw, commit)
	}
	dir := c.CheckoutDir(src, commit)
	if c.HasCheckout(src, commit) {
		return dir, nil
	}
	repo, err := c.ensureRepository(ctx, src)
	if err != nil {
		return "", err
	}
	if err := c.ensureCommit(ctx, src, repo, commit); err != nil {
		return "", err
	}
	return c.materialize(ctx, src, repo, commit, dir)
}

func (c *Cache) ensureRepository(ctx context.Context, src Source) (string, error) {
	repo := c.RepoDir(src)
	if info, err := os.Stat(repo); err == nil && info.IsDir() {
		return repo, nil
	}
	parent := filepath.Dir(repo)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("git source %s: %w", src.Raw, err)
	}
	staging, err := os.MkdirTemp(parent, ".repo.git.clone-*")
	if err != nil {
		return "", fmt.Errorf("git source %s: %w", src.Raw, err)
	}
	defer os.RemoveAll(staging)
	// A blobless clone keeps the cache small when the server supports it; a
	// server that does not answers with an error rather than a warning on
	// some transports, so the plain bare clone is the fallback.
	if _, err := c.git(ctx, src, "", "clone", "--bare", "--quiet", "--filter=blob:none", src.URL, staging); err != nil {
		if errors.Is(err, ErrGitNotFound) {
			return "", err
		}
		_ = os.RemoveAll(staging)
		if _, err := c.git(ctx, src, "", "clone", "--bare", "--quiet", src.URL, staging); err != nil {
			return "", err
		}
	}
	if err := os.Rename(staging, repo); err != nil {
		if info, statErr := os.Stat(repo); statErr == nil && info.IsDir() {
			return repo, nil // another process won the race
		}
		return "", fmt.Errorf("git source %s: %w", src.Raw, err)
	}
	return repo, nil
}

func (c *Cache) hasCommit(ctx context.Context, src Source, repo, commit string) bool {
	_, err := c.git(ctx, src, repo, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

func (c *Cache) ensureCommit(ctx context.Context, src Source, repo, commit string) error {
	if c.hasCommit(ctx, src, repo, commit) {
		return nil
	}
	// Fetching a bare commit id is allowed by protocol v2 and by servers that
	// permit reachable ids; a server that refuses it gets the whole set of
	// heads and tags instead.
	if _, err := c.git(ctx, src, repo, "fetch", "--quiet", "--no-tags", src.URL, commit); err != nil {
		if errors.Is(err, ErrGitNotFound) {
			return err
		}
		if _, err := c.git(ctx, src, repo, "fetch", "--quiet", "--prune", src.URL,
			"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"); err != nil {
			return err
		}
	}
	if !c.hasCommit(ctx, src, repo, commit) {
		return fmt.Errorf("git source %s: commit %s is not in %s", src.Raw, commit, src.URL)
	}
	return nil
}

func (c *Cache) materialize(ctx context.Context, src Source, repo, commit, dir string) (string, error) {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("git source %s: %w", src.Raw, err)
	}
	staging, err := os.MkdirTemp(parent, "."+commit+".checkout-*")
	if err != nil {
		return "", fmt.Errorf("git source %s: %w", src.Raw, err)
	}
	defer os.RemoveAll(staging)
	index := staging + ".index"
	defer os.Remove(index)
	_, err = c.gitEnv(ctx, src, "", []string{"GIT_INDEX_FILE=" + index},
		"--git-dir="+repo, "--work-tree="+staging, "checkout", "--quiet", commit, "--", ".")
	if err != nil {
		return "", err
	}
	if err := os.Rename(staging, dir); err != nil {
		if c.HasCheckout(src, commit) {
			return dir, nil // another process won the race
		}
		return "", fmt.Errorf("git source %s: %w", src.Raw, err)
	}
	return dir, nil
}

func (c *Cache) git(ctx context.Context, src Source, dir string, args ...string) (string, error) {
	return c.gitEnv(ctx, src, dir, nil, args...)
}

// gitEnv runs the system git. dir, when set, is passed as -C so the command
// runs inside the bare clone. The environment is inherited: authentication is
// git's own (credential helpers, the ssh agent, GIT_* variables).
func (c *Cache) gitEnv(ctx context.Context, src Source, dir string, env []string, args ...string) (string, error) {
	binary, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("%w (needed for %s)", ErrGitNotFound, src.Raw)
	}
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git source %s: git %s: %s", src.Raw, strings.Join(args, " "), detail)
	}
	return stdout.String(), nil
}
