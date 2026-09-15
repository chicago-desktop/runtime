// SPDX-License-Identifier: MPL-2.0

package cmd

import (
	"context"
	"fmt"

	"github.com/wippyai/runtime/boot/deps/gitsource"
	"github.com/wippyai/runtime/boot/deps/hub"
	"github.com/wippyai/runtime/boot/deps/lock"
	"github.com/wippyai/runtime/cmd/internal/entries"
	"go.uber.org/zap"
)

// lockModuleFromResolved renders a resolved module as its lock row: a Hub
// module pins its artifact digest, a git module its repository, commit and
// tree digest.
func lockModuleFromResolved(m hub.ResolvedModule) lock.Module {
	row := lock.Module{
		Name:    m.Org + "/" + m.Name,
		Version: m.Version,
	}
	if m.Source == "git" {
		row.Source = m.Repository
		row.Commit = m.Commit
		row.LocalHash = m.Digest
		return row
	}
	row.Hash = m.Digest
	return row
}

// resolveGitReplacements resolves the ref of every git replacement to a
// commit, checks the commit out and binds the replacement to that checkout.
// This is the only place a replacement's branch moves: install and the boot
// take the commit from the lock.
func resolveGitReplacements(ctx context.Context, lockObj *lock.Lock, logger *zap.Logger) error {
	if lockObj == nil {
		return nil
	}
	cache := lockObj.GitCache()
	for _, repl := range lockObj.GitReplacements() {
		src, err := gitsource.Parse(repl.Source)
		if err != nil {
			return NewBuildDependencyGraphError(fmt.Errorf("replacement %s: %w", repl.From, err))
		}
		if src.Ref == "" {
			return NewBuildDependencyGraphError(fmt.Errorf(
				"replacement %s: %s names no ref; write url#branch, url#tag or url#commit", repl.From, repl.Source))
		}
		commit, err := cache.ResolveRef(ctx, src, src.Ref)
		if err != nil {
			return NewBuildDependencyGraphError(err)
		}
		if _, err := cache.Checkout(ctx, src, commit); err != nil {
			return NewBuildDependencyGraphError(err)
		}
		dir, ok := lockObj.BindGitReplacement(repl.From, commit)
		if !ok {
			return NewBuildDependencyGraphError(fmt.Errorf("replacement %s is not a workspace replacement", repl.From))
		}
		if logger != nil {
			logger.Info("resolved git replacement",
				zap.String("module", repl.From),
				zap.String("source", repl.Source),
				zap.String("commit", commit),
				zap.String("checkout", dir))
		}
	}
	return nil
}

// recordGitReplacements writes the source, commit and tree digest of every
// bound git replacement of source into the matching module rows of target,
// so the lock carries what the boot needs to find the checkout offline.
func recordGitReplacements(target, source *lock.Lock) error {
	if target == nil || source == nil {
		return nil
	}
	for _, repl := range source.GitReplacements() {
		if repl.Commit == "" || repl.To == "" {
			continue
		}
		module, ok := target.GetModule(repl.From)
		if !ok {
			continue
		}
		digest, _, err := hub.ReplacementTreeIdentity(repl.To)
		if err != nil {
			return NewWriteLockFileError(fmt.Errorf("hash git replacement %s: %w", repl.From, err))
		}
		module.Hash = ""
		module.Source = repl.Source
		module.Commit = repl.Commit
		module.LocalHash = digest
		target.SetModule(module)
	}
	return nil
}

// ensureGitModules materializes and verifies the checkout of every git
// module row of the lock (dependencies and replacements alike) and returns
// how many there were. A commit already checked out needs no git.
func ensureGitModules(ctx context.Context, lockObj *lock.Lock, logger *zap.Logger) (int, error) {
	count := 0
	for _, module := range lockObj.GetModules() {
		if !module.IsGit() {
			continue
		}
		count++
		if err := entries.EnsureGitModuleCheckout(ctx, lockObj, module, logger); err != nil {
			return count, NewStoreModuleError(module.Name+"@"+module.Version, err)
		}
	}
	return count, nil
}
