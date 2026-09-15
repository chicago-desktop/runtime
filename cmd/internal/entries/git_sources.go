// SPDX-License-Identifier: MPL-2.0

package entries

import (
	"context"
	"fmt"
	"strings"

	"github.com/wippyai/runtime/api/payload"
	regapi "github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/boot/deps/gitsource"
	"github.com/wippyai/runtime/boot/deps/hub"
	"github.com/wippyai/runtime/boot/deps/lock"
	"go.uber.org/zap"
)

// EnsureGitModuleCheckout materializes the checkout a git module row of the
// lock names and verifies its tree against local_hash. A commit already
// checked out costs no git call; a missing one is fetched. A tree that
// differs from the lock is refused, as a changed directory replacement is.
func EnsureGitModuleCheckout(ctx context.Context, lockObj *lock.Lock, mod lock.Module, logger *zap.Logger) error {
	src, err := gitsource.Parse(mod.Source)
	if err != nil {
		return NewModuleIntegrityError(mod.Name, err)
	}
	cache := lockObj.GitCache()
	dir := cache.CheckoutDir(src, mod.Commit)
	if !cache.HasCheckout(src, mod.Commit) {
		logger.Info("checking out git module",
			zap.String("module", mod.Name),
			zap.String("source", mod.Source),
			zap.String("commit", mod.Commit))
		if dir, err = cache.Checkout(ctx, src, mod.Commit); err != nil {
			return NewDownloadModuleError(mod.Name+"@"+mod.Version, err)
		}
	}
	return VerifyGitCheckout(dir, mod)
}

// VerifyGitCheckout compares a checkout's tree digest with the lock row.
func VerifyGitCheckout(dir string, mod lock.Module) error {
	digest, _, err := hub.ReplacementTreeIdentity(dir)
	if err != nil {
		return NewModuleIntegrityError(mod.Name, err)
	}
	if !strings.EqualFold(digest, mod.LocalHash) {
		return NewModuleIntegrityError(mod.Name, fmt.Errorf(
			"git checkout %s@%s tree digest %s does not match lock local_hash %s",
			mod.Source, mod.Commit, digest, mod.LocalHash))
	}
	return nil
}

// canonicalizeGitComponents rewrites the component of every ns.dependency
// entry that names a git repository to the module the lock resolved that
// repository to, so linking and the registry see organization/module. A
// repository the lock does not know stays as written for the resolver to
// refuse with its own message.
func canonicalizeGitComponents(entries []regapi.Entry, gitModules map[string]string, dtt payload.Transcoder) error {
	if len(gitModules) == 0 {
		return nil
	}
	keys := make(map[string]string, len(gitModules))
	for source, module := range gitModules {
		src, err := gitsource.Parse(source)
		if err != nil {
			continue
		}
		keys[src.Key] = module
	}
	for i := range entries {
		entry := &entries[i]
		if entry.Kind != regapi.NamespaceDependency {
			continue
		}
		var data map[string]any
		if err := dtt.Unmarshal(entry.Data, &data); err != nil {
			return fmt.Errorf("decode dependency %s: %w", entry.ID.String(), err)
		}
		component, _ := data["component"].(string)
		if !gitsource.IsSource(component) {
			continue
		}
		src, err := gitsource.Parse(component)
		if err != nil {
			return fmt.Errorf("dependency %s: %w", entry.ID.String(), err)
		}
		module, ok := keys[src.Key]
		if !ok {
			continue
		}
		data["component"] = module
		entry.Data = payload.New(data)
	}
	return nil
}
