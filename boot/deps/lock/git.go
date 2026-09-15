// SPDX-License-Identifier: MPL-2.0

package lock

import (
	"path/filepath"

	"github.com/wippyai/runtime/boot/deps/gitsource"
)

// GitCacheRoot returns the git cache root this lock derives checkout paths
// from: the configured root, else WIPPY_GIT_CACHE, else $HOME/.wippy/git,
// else <modules dir>/git next to the lock.
func (l *Lock) GitCacheRoot() string {
	if l.gitCache != "" {
		return l.gitCache
	}
	modulesDir := l.data.Directories.Modules
	if modulesDir == "" {
		modulesDir = ".wippy"
	}
	return gitsource.DefaultRoot(filepath.Join(ResolveLockPath(filepath.Dir(l.path), modulesDir), "git"))
}

// GitCache returns the cache the lock's git modules are checked out in.
func (l *Lock) GitCache() *gitsource.Cache {
	return gitsource.New(l.GitCacheRoot())
}

// GitCheckoutDir returns where mod's commit is (or will be) checked out.
// False when mod is not a git module, its source does not parse or it has no
// commit; whether the directory exists is the caller's question.
func (l *Lock) GitCheckoutDir(mod Module) (string, bool) {
	if !mod.IsGit() || mod.Commit == "" {
		return "", false
	}
	src, err := gitsource.Parse(mod.Source)
	if err != nil {
		return "", false
	}
	return l.GitCache().CheckoutDir(src, mod.Commit), true
}

// ModuleForSource returns the locked module taken from source, comparing
// repositories rather than spellings: github.com/x/y and git@github.com:x/y.git
// name one repository. This is how a dependency written as a repository
// finds its module name offline.
func (l *Lock) ModuleForSource(source string) (Module, bool) {
	wanted, err := gitsource.Parse(source)
	if err != nil {
		return Module{}, false
	}
	for _, mod := range l.data.Modules {
		if !mod.IsGit() {
			continue
		}
		have, err := gitsource.Parse(mod.Source)
		if err != nil {
			continue
		}
		if have.SameRepository(wanted) {
			return mod, true
		}
	}
	return Module{}, false
}

// GitReplacements returns the effective replacements that name a git source.
func (l *Lock) GitReplacements() []Replacement {
	var out []Replacement
	for _, repl := range l.effectiveReplacements() {
		if repl.IsGit() {
			out = append(out, repl)
		}
	}
	return out
}

// BindGitReplacement records the commit a git replacement's ref resolved to
// and points the replacement at that commit's checkout. wippy update calls
// it after resolving the ref; the lock's module row is written from the
// resulting Replacement.Commit.
func (l *Lock) BindGitReplacement(from, commit string) (string, bool) {
	for i := range l.workspaceOverlay {
		repl := &l.workspaceOverlay[i]
		if repl.From != from || !repl.IsGit() {
			continue
		}
		src, err := gitsource.Parse(repl.Source)
		if err != nil {
			return "", false
		}
		repl.Commit = commit
		repl.To = l.GitCache().CheckoutDir(src, commit)
		return repl.To, true
	}
	return "", false
}

// bindGitReplacements points every git replacement whose module the lock
// already records (same repository, with a commit) at that commit's
// checkout. A replacement the lock knows nothing about keeps an empty To:
// the boot refuses it and wippy update resolves it.
func (l *Lock) bindGitReplacements() {
	for i := range l.workspaceOverlay {
		repl := &l.workspaceOverlay[i]
		if !repl.IsGit() || repl.To != "" {
			continue
		}
		src, err := gitsource.Parse(repl.Source)
		if err != nil {
			continue
		}
		mod, ok := l.GetModule(repl.From)
		if !ok || !mod.IsGit() || mod.Commit == "" {
			continue
		}
		have, err := gitsource.Parse(mod.Source)
		if err != nil || !have.SameRepository(src) {
			continue
		}
		repl.Commit = mod.Commit
		repl.To = l.GitCache().CheckoutDir(src, mod.Commit)
	}
}
