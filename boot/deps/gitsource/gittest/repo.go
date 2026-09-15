// SPDX-License-Identifier: MPL-2.0

// Package gittest builds small bare git repositories for tests: a module
// tree committed at a few versions and tagged, with no network anywhere.
package gittest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Repo is a bare repository plus the working clone that feeds it.
type Repo struct {
	// Bare is the repository's path, usable as a git source (with "#ref").
	Bare string
	// Work is the clone commits are made in.
	Work string
	// Commits maps every tag and pushed branch to its commit id.
	Commits map[string]string
	org     string
	module  string
}

// New creates an empty bare repository and a clone for the module
// organization/module. Tests that need git skip when it is not installed.
func New(t *testing.T, org, module string) *Repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	r := &Repo{
		Bare:    filepath.Join(root, module+".git"),
		Work:    filepath.Join(root, module),
		Commits: make(map[string]string),
		org:     org,
		module:  module,
	}
	r.Git(t, root, "init", "--quiet", "--bare", "-b", "main", r.Bare)
	r.Git(t, root, "init", "--quiet", "-b", "main", r.Work)
	return r
}

// Git runs git in dir with a fixed identity and fails the test on error.
func (r *Repo) Git(t *testing.T, dir string, args ...string) string {
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

// Commit writes wippy.yaml at version plus the given files (paths relative
// to the module root) and commits. Files are replaced, not merged.
func (r *Repo) Commit(t *testing.T, version string, files map[string]string) string {
	t.Helper()
	manifest := "organization: " + r.org + "\nmodule: " + r.module + "\nversion: " + version + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(r.Work, "wippy.yaml"), []byte(manifest), 0o600))
	for name, content := range files {
		path := filepath.Join(r.Work, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	r.Git(t, r.Work, "add", "--all")
	r.Git(t, r.Work, "commit", "--quiet", "--allow-empty", "-m", "version "+version)
	return r.Git(t, r.Work, "rev-parse", "HEAD")
}

// Tag tags HEAD; annotated tags exercise the peeled path of ls-remote.
func (r *Repo) Tag(t *testing.T, name string, annotated bool) {
	t.Helper()
	if annotated {
		r.Git(t, r.Work, "tag", "-a", name, "-m", name)
	} else {
		r.Git(t, r.Work, "tag", name)
	}
	r.Commits[name] = r.Git(t, r.Work, "rev-parse", name+"^{commit}")
}

// Push pushes main and every tag to the bare repository.
func (r *Repo) Push(t *testing.T) {
	t.Helper()
	r.Git(t, r.Work, "push", "--quiet", "--force", r.Bare, "main", "--tags")
	r.Commits["main"] = r.Git(t, r.Work, "rev-parse", "HEAD")
}

// Source renders the repository as a git source, with ref when given.
func (r *Repo) Source(ref string) string {
	if ref == "" {
		return r.Bare
	}
	return r.Bare + "#" + ref
}

// Name returns organization/module.
func (r *Repo) Name() string {
	return r.org + "/" + r.module
}

// Index renders a src/_index.json body with the given entries.
func Index(namespace string, entries ...string) string {
	return `{"namespace": "` + namespace + `", "entries": [` + strings.Join(entries, ",") + `]}`
}

// Dependency renders an ns.dependency entry.
func Dependency(name, component, version string) string {
	return `{"name": "` + name + `", "kind": "ns.dependency", "component": "` + component + `", "version": "` + version + `"}`
}
