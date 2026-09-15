// SPDX-License-Identifier: MPL-2.0

package hub

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/wippyai/runtime/api/payload"
	regapi "github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/api/semver"
	depconfig "github.com/wippyai/runtime/boot/deps/config"
	"github.com/wippyai/runtime/boot/deps/gitsource"
	"github.com/wippyai/runtime/boot/deps/graph"
)

// moduleSourceGit marks a module taken from a git repository at a commit.
// Its digest is the checkout's tree digest, as for a directory replacement.
const moduleSourceGit = "git"

// gitBinding ties one repository to the module it contains. A dependency
// names the repository; the resolver, the lock and every other module name
// the module, so the binding is made once per repository and reused.
type gitBinding struct {
	source  gitsource.Source
	name    string            // organization/module from the manifest
	commits map[string]string // version -> commit
	tags    map[string]string // version -> tag name, when the tags were listed
	listed  bool
}

// gitState is the handler's view of git sources: the cache, the bindings by
// repository and by module name, and whether tags are listed again for
// repositories the lock already binds (wippy update) or taken from the lock.
type gitState struct {
	cache   *gitsource.Cache
	byKey   map[string]*gitBinding
	byName  map[string]*gitBinding
	mu      sync.Mutex
	refresh bool
}

func newGitState(cache *gitsource.Cache, refresh bool) *gitState {
	return &gitState{
		cache:   cache,
		byKey:   make(map[string]*gitBinding),
		byName:  make(map[string]*gitBinding),
		refresh: refresh,
	}
}

// normalizeGitVersion turns a tag into the version the lock records: the
// tag without a leading v.
func normalizeGitVersion(tag string) string {
	return strings.TrimPrefix(strings.TrimSpace(tag), "v")
}

// gitSemverTags keeps the tags that parse as semantic versions, keyed by
// normalized version; a tag and its v-prefixed twin are one version and the
// bare spelling wins.
func gitSemverTags(tags []gitsource.Tag) map[string]gitsource.Tag {
	versions := make(map[string]gitsource.Tag, len(tags))
	for _, tag := range tags {
		if _, err := semver.ParseVersion(tag.Name); err != nil {
			continue
		}
		version := normalizeGitVersion(tag.Name)
		if existing, ok := versions[version]; ok && !strings.HasPrefix(existing.Name, "v") {
			continue
		}
		versions[version] = tag
	}
	return versions
}

// selectGitVersion picks the highest version among tags that satisfies
// constraint, a stable release before a prerelease as the resolver does.
// An empty constraint or "*" takes the highest tag.
func selectGitVersion(tags map[string]gitsource.Tag, constraint string) (string, error) {
	constraint = strings.TrimSpace(constraint)
	if constraint == "" {
		constraint = "*"
	}
	if !semver.IsConstraint(constraint) {
		exact := normalizeGitVersion(strings.TrimPrefix(constraint, "="))
		if _, ok := tags[exact]; ok {
			return exact, nil
		}
		return "", fmt.Errorf("version %q is not a tag; tags seen: %s", constraint, describeGitTags(tags))
	}
	parsed, err := semver.ParseConstraint(constraint)
	if err != nil {
		return "", fmt.Errorf("invalid constraint %q: %w", constraint, err)
	}
	candidates := make([]semver.Version, 0, len(tags))
	for version := range tags {
		parsedVersion, err := semver.ParseVersion(version)
		if err != nil {
			continue
		}
		candidates = append(candidates, parsedVersion)
	}
	best, err := parsed.FindBestMatch(candidates)
	if err != nil {
		return "", fmt.Errorf("no tag satisfies %q; tags seen: %s", constraint, describeGitTags(tags))
	}
	for version := range tags {
		if parsedVersion, err := semver.ParseVersion(version); err == nil && parsedVersion.Equal(best) {
			return version, nil
		}
	}
	return best.String(), nil
}

func describeGitTags(tags map[string]gitsource.Tag) string {
	if len(tags) == 0 {
		return "none (a branch is not a version; pin one through a replacement)"
	}
	names := make([]string, 0, len(tags))
	for _, tag := range tags {
		names = append(names, tag.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// gitModuleName reads organization/module from the checkout's wippy.yaml.
func gitModuleName(dir string, src gitsource.Source) (string, error) {
	cfg, err := depconfig.Load(dir)
	if err != nil {
		return "", fmt.Errorf("git source %s: %w", src.Raw, err)
	}
	org := strings.TrimSpace(cfg.Organization)
	module := strings.TrimSpace(cfg.ModuleName)
	if org == "" || module == "" {
		return "", fmt.Errorf("git source %s: wippy.yaml declares no organization/module", src.Raw)
	}
	name := graph.Name{Organization: org, Module: module}
	if err := validateModuleArtifactIdentity(name, "0.0.0", ""); err != nil {
		return "", fmt.Errorf("git source %s: %w", src.Raw, err)
	}
	return name.String(), nil
}

// gitState returns the handler's git state, creating a default one for a
// handler built without NewDependencyHandler.
func (h *DependencyHandler) gitState() *gitState {
	gitStateInit.Lock()
	defer gitStateInit.Unlock()
	if h.git == nil {
		root := gitsource.DefaultRoot(filepath.Join(filepath.Dir(h.vendorDir), "git"))
		if h.lock != nil {
			root = h.lock.GitCacheRoot()
		}
		h.git = newGitState(gitsource.New(root), false)
	}
	return h.git
}

var gitStateInit sync.Mutex

// gitCache returns the handler's git cache.
func (h *DependencyHandler) gitCache() *gitsource.Cache {
	return h.gitState().cache
}

// isGitModule reports whether name is taken from git in this handler's view:
// bound during this resolution, recorded in the lock, or recorded in the
// deployment. A replaced module is a directory, whatever its origin.
func (h *DependencyHandler) isGitModule(name string) bool {
	if h == nil {
		return false
	}
	if _, replaced := h.replacementPath(name); replaced {
		return false
	}
	h.gitState().mu.Lock()
	_, bound := h.gitState().byName[name]
	h.gitState().mu.Unlock()
	if bound {
		return true
	}
	if h.lock != nil {
		if mod, ok := h.lock.GetModule(name); ok && mod.IsGit() {
			return true
		}
	}
	if h.deployment != nil {
		for _, mod := range h.deployment.Modules {
			if mod.Name == name {
				return mod.Source == moduleSourceGit
			}
		}
	}
	return false
}

// decodeDependency decodes an ns.dependency entry and canonicalizes a git
// component to the module's name, so the resolver, the registry and every
// comparison against a selected module see organization/module.
func (h *DependencyHandler) decodeDependency(ctx context.Context, transcoder payload.Transcoder, entry regapi.Entry) (DependencyDefinition, error) {
	def, err := decodeDependency(ctx, transcoder, entry)
	if err != nil {
		return DependencyDefinition{}, err
	}
	return h.canonicalizeDependency(ctx, def)
}

// canonicalizeDependency rewrites a git component to the module name it
// binds to. A Hub component is returned unchanged.
func (h *DependencyHandler) canonicalizeDependency(ctx context.Context, def DependencyDefinition) (DependencyDefinition, error) {
	if !gitsource.IsSource(def.Component) {
		return def, nil
	}
	binding, err := h.bindGitSource(ctx, def.Component, def.Version)
	if err != nil {
		return DependencyDefinition{}, err
	}
	def.Component = binding.name
	return def, nil
}

func (h *DependencyHandler) canonicalizeDependencies(ctx context.Context, deps []DependencyDefinition) ([]DependencyDefinition, error) {
	out := make([]DependencyDefinition, 0, len(deps))
	for _, dep := range deps {
		canonical, err := h.canonicalizeDependency(ctx, dep)
		if err != nil {
			return nil, err
		}
		out = append(out, canonical)
	}
	return out, nil
}

// bindGitSource finds the module a repository contains. Offline evidence
// (the lock, the deployment, the current resolution) answers without git
// unless the handler is refreshing; otherwise the tags are listed, the
// highest tag satisfying constraint is checked out and its manifest read.
func (h *DependencyHandler) bindGitSource(ctx context.Context, component, constraint string) (*gitBinding, error) {
	src, err := gitsource.Parse(component)
	if err != nil {
		return nil, NewDependencyEntryInvalidError("", err.Error(), component)
	}
	h.gitState().mu.Lock()
	defer h.gitState().mu.Unlock()

	if binding := h.gitState().byKey[src.Key]; binding != nil {
		return binding, nil
	}
	if !h.gitState().refresh {
		if binding := h.recordedGitBinding(ctx, src); binding != nil {
			return h.registerGitBinding(binding)
		}
	}
	if offlineStartup(ctx) {
		return nil, NewDependencyOfflineError("resolve git source", component)
	}

	tags, err := h.gitCache().Tags(ctx, src)
	if err != nil {
		return nil, NewDependencyResolutionError(err)
	}
	versions := gitSemverTags(tags)
	version, err := selectGitVersion(versions, constraint)
	if err != nil {
		return nil, NewDependencyResolutionError(fmt.Errorf("git source %s: %w", src.Raw, err))
	}
	dir, err := h.gitCache().Checkout(ctx, src, versions[version].Commit)
	if err != nil {
		return nil, NewDependencyResolutionError(err)
	}
	name, err := gitModuleName(dir, src)
	if err != nil {
		return nil, NewDependencyResolutionError(err)
	}
	binding := &gitBinding{
		source:  src,
		name:    name,
		commits: make(map[string]string, len(versions)),
		tags:    make(map[string]string, len(versions)),
		listed:  true,
	}
	for v, tag := range versions {
		binding.commits[v] = tag.Commit
		binding.tags[v] = tag.Name
	}
	return h.registerGitBinding(binding)
}

// recordedGitBinding binds a repository from what the lock, the deployment
// or the current resolution already recorded. Needs h.gitState().mu.
func (h *DependencyHandler) recordedGitBinding(ctx context.Context, src gitsource.Source) *gitBinding {
	if h.lock != nil {
		if mod, ok := h.lock.ModuleForSource(src.Raw); ok && mod.Commit != "" {
			return &gitBinding{
				source:  src,
				name:    mod.Name,
				commits: map[string]string{mod.Version: mod.Commit},
			}
		}
	}
	var records []regapi.ResolvedModule
	if h.deployment != nil {
		records = append(records, h.deployment.Modules...)
	}
	if resolution := h.currentResolution(ctx); resolution != nil {
		records = append(records, resolution.Modules...)
	}
	for _, record := range records {
		if record.Source != moduleSourceGit || record.Commit == "" {
			continue
		}
		recorded, err := gitsource.Parse(record.Repository)
		if err != nil || !recorded.SameRepository(src) {
			continue
		}
		return &gitBinding{
			source:  src,
			name:    record.Name,
			commits: map[string]string{record.Version: record.Commit},
		}
	}
	return nil
}

// registerGitBinding indexes a binding, refusing a second repository for one
// module name. Needs h.gitState().mu.
func (h *DependencyHandler) registerGitBinding(binding *gitBinding) (*gitBinding, error) {
	if existing := h.gitState().byName[binding.name]; existing != nil && !existing.source.SameRepository(binding.source) {
		return nil, NewDependencyResolutionError(fmt.Errorf(
			"module %s is provided by two git sources: %s and %s",
			binding.name, existing.source.Raw, binding.source.Raw,
		))
	}
	h.gitState().byKey[binding.source.Key] = binding
	h.gitState().byName[binding.name] = binding
	return binding, nil
}

func (h *DependencyHandler) gitBindingByName(name string) *gitBinding {
	h.gitState().mu.Lock()
	defer h.gitState().mu.Unlock()
	return h.gitState().byName[name]
}

// gitBindingCommit returns the commit a version of a bound module resolves
// to: from the binding's tags, the lock, or — online — a fresh tag listing.
func (h *DependencyHandler) gitBindingCommit(ctx context.Context, binding *gitBinding, version string) (string, error) {
	h.gitState().mu.Lock()
	defer h.gitState().mu.Unlock()
	if commit, ok := binding.commits[version]; ok {
		return commit, nil
	}
	if h.lock != nil {
		if mod, ok := h.lock.GetModule(binding.name); ok && mod.IsGit() && normalizeGitVersion(mod.Version) == version && mod.Commit != "" {
			binding.commits[version] = mod.Commit
			return mod.Commit, nil
		}
	}
	if !binding.listed && !offlineStartup(ctx) {
		if err := h.listGitTagsLocked(ctx, binding); err != nil {
			return "", err
		}
		if commit, ok := binding.commits[version]; ok {
			return commit, nil
		}
	}
	return "", fmt.Errorf("git source %s: version %s is not a tag", binding.source.Raw, version)
}

// listGitTagsLocked lists the repository's tags into the binding. Needs h.gitState().mu.
func (h *DependencyHandler) listGitTagsLocked(ctx context.Context, binding *gitBinding) error {
	tags, err := h.gitCache().Tags(ctx, binding.source)
	if err != nil {
		return err
	}
	if binding.tags == nil {
		binding.tags = make(map[string]string)
	}
	for version, tag := range gitSemverTags(tags) {
		binding.commits[version] = tag.Commit
		binding.tags[version] = tag.Name
	}
	binding.listed = true
	return nil
}

// gitCheckout returns the checkout directory of commit, materializing it
// unless the startup is verified offline, where a missing checkout is a
// missing-evidence failure rather than a network call.
func (h *DependencyHandler) gitCheckout(ctx context.Context, src gitsource.Source, commit, module string) (string, error) {
	cache := h.gitCache()
	if cache.HasCheckout(src, commit) {
		return cache.CheckoutDir(src, commit), nil
	}
	if offlineStartup(ctx) {
		return "", NewDependencyOfflineError("checkout "+src.Raw+"@"+commit, module)
	}
	dir, err := cache.Checkout(ctx, src, commit)
	if err != nil {
		return "", NewDependencyDownloadError(module, err)
	}
	return dir, nil
}

// gitModuleIdentity returns the repository and commit recorded for a git
// module, from the resolved module itself or from the lock.
func (h *DependencyHandler) gitModuleIdentity(mod ResolvedModule) (gitsource.Source, string, error) {
	name := mod.Org + "/" + mod.Name
	repository, commit := mod.Repository, mod.Commit
	if (repository == "" || commit == "") && h.lock != nil {
		if locked, ok := h.lock.GetModule(name); ok && locked.IsGit() && normalizeGitVersion(locked.Version) == normalizeGitVersion(mod.Version) {
			repository, commit = locked.Source, locked.Commit
		}
	}
	if repository == "" || commit == "" {
		if binding := h.gitBindingByName(name); binding != nil {
			h.gitState().mu.Lock()
			bound, ok := binding.commits[normalizeGitVersion(mod.Version)]
			h.gitState().mu.Unlock()
			if ok {
				repository, commit = binding.source.Raw, bound
			}
		}
	}
	if repository == "" || commit == "" {
		return gitsource.Source{}, "", fmt.Errorf("git module %s has no recorded repository and commit; run wippy update", modKey(mod))
	}
	src, err := gitsource.Parse(repository)
	if err != nil {
		return gitsource.Source{}, "", err
	}
	return src, commit, nil
}

// gitCheckoutPath returns the checkout directory of a git module without
// touching the network; false when it is not materialized.
func (h *DependencyHandler) gitCheckoutPath(mod ResolvedModule) (string, bool) {
	src, commit, err := h.gitModuleIdentity(mod)
	if err != nil || !h.gitCache().HasCheckout(src, commit) {
		return "", false
	}
	return h.gitCache().CheckoutDir(src, commit), true
}

// ensureGitModuleAvailable materializes a git module's checkout and verifies
// its tree against the recorded digest: a checkout whose tree differs from
// the lock is refused, as a changed directory replacement is.
func (h *DependencyHandler) ensureGitModuleAvailable(ctx context.Context, mod ResolvedModule) (string, error) {
	src, commit, err := h.gitModuleIdentity(mod)
	if err != nil {
		return "", NewDependencyLoadError(modKey(mod), err)
	}
	dir, err := h.gitCheckout(ctx, src, commit, modKey(mod))
	if err != nil {
		return "", err
	}
	digest, size, err := digestReplacementTree(dir)
	if err != nil {
		return "", NewDependencyIntegrityError(modKey(mod), err, mod.Digest, mod.SizeBytes)
	}
	if mod.Digest != "" && !strings.EqualFold(mod.Digest, digest) {
		return "", NewDependencyIntegrityError(modKey(mod),
			fmt.Errorf("git checkout %s@%s tree digest mismatch", src.Raw, commit), mod.Digest, mod.SizeBytes)
	}
	if mod.SizeBytes > 0 && mod.SizeBytes != size {
		return "", NewDependencyIntegrityError(modKey(mod),
			fmt.Errorf("git checkout %s@%s tree size mismatch", src.Raw, commit), mod.Digest, mod.SizeBytes)
	}
	return dir, nil
}

// completeGitModuleIdentities stamps every resolved module bound to a git
// source with its origin and commit, so the lock and the durable resolution
// record where the tree came from.
func (h *DependencyHandler) completeGitModuleIdentities(ctx context.Context, modules []ResolvedModule) error {
	for i := range modules {
		mod := &modules[i]
		name := mod.Org + "/" + mod.Name
		if _, replaced := h.replacementPath(name); replaced {
			continue
		}
		binding := h.gitBindingByName(name)
		if binding == nil {
			continue
		}
		version := normalizeGitVersion(mod.Version)
		commit, err := h.gitBindingCommit(ctx, binding, version)
		if err != nil {
			return NewDependencyResolutionError(err)
		}
		mod.Source = moduleSourceGit
		mod.Repository = binding.source.Raw
		mod.Commit = commit
		mod.VersionID = ""
		mod.URL = ""
		if mod.Digest == "" || mod.SizeBytes == 0 {
			dir, err := h.gitCheckout(ctx, binding.source, commit, modKey(*mod))
			if err != nil {
				return err
			}
			digest, size, err := digestReplacementTree(dir)
			if err != nil {
				return NewDependencyIntegrityError(modKey(*mod), err, mod.Digest, mod.SizeBytes)
			}
			mod.Digest = digest
			mod.SizeBytes = size
		}
	}
	return nil
}

// gitManifestProvider answers for modules bound to a git repository: the
// versions are the repository's semver tags and a manifest is the checkout
// of the tag's commit, read exactly as a directory replacement is. Modules
// without a binding fall through to the base provider.
type gitManifestProvider struct {
	base    ManifestProvider
	handler *DependencyHandler
}

func (p *gitManifestProvider) ListAllVersions(ctx context.Context, org, module string) ([]VersionInfo, error) {
	name := org + "/" + module
	binding := p.handler.gitBindingByName(name)
	if binding == nil {
		return p.base.ListAllVersions(ctx, org, module)
	}
	p.handler.gitState().mu.Lock()
	defer p.handler.gitState().mu.Unlock()
	if !binding.listed && !offlineStartup(ctx) {
		if err := p.handler.listGitTagsLocked(ctx, binding); err != nil {
			return nil, err
		}
	}
	versions := make([]VersionInfo, 0, len(binding.commits))
	for version := range binding.commits {
		versions = append(versions, VersionInfo{Version: version})
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Version < versions[j].Version })
	return versions, nil
}

func (p *gitManifestProvider) GetManifest(ctx context.Context, org, module, constraint string) (*ModuleManifest, error) {
	name := org + "/" + module
	binding := p.handler.gitBindingByName(name)
	if binding == nil {
		return p.base.GetManifest(ctx, org, module, constraint)
	}
	version := normalizeGitVersion(strings.TrimPrefix(strings.TrimSpace(constraint), "="))
	if version == "" || semver.IsConstraint(version) {
		return nil, fmt.Errorf("git source %s: manifest requested for range %q rather than a version", binding.source.Raw, constraint)
	}
	commit, err := p.handler.gitBindingCommit(ctx, binding, version)
	if err != nil {
		return nil, err
	}
	dir, err := p.handler.gitCheckout(ctx, binding.source, commit, name+"@"+version)
	if err != nil {
		return nil, err
	}
	declared, err := gitModuleName(dir, binding.source)
	if err != nil {
		return nil, err
	}
	if declared != name {
		return nil, fmt.Errorf("git source %s at %s declares module %s, expected %s", binding.source.Raw, version, declared, name)
	}
	transcoder := payload.GetTranscoder(ctx)
	if transcoder == nil {
		return nil, ErrDependencyTranscoderMissing
	}
	entries, err := loadReplacementEntries(ctx, dir, p.handler.logger, transcoder)
	if err != nil {
		return nil, err
	}
	entries, err = p.handler.applyModuleConfigFilters(ctx, dir, entries)
	if err != nil {
		return nil, err
	}
	dependencies, err := manifestDependenciesFromEntries(ctx, p.handler, transcoder, entries)
	if err != nil {
		return nil, err
	}
	digest, size, err := digestReplacementTree(dir)
	if err != nil {
		return nil, err
	}
	return &ModuleManifest{
		Org:          org,
		Name:         module,
		Version:      version,
		Digest:       digest,
		SizeBytes:    size,
		Dependencies: dependencies,
	}, nil
}

// gitReplacementCheckout materializes a git replacement whose checkout is
// absent: the lock bound it to a commit, but nothing has checked it out yet.
func (h *DependencyHandler) gitReplacementCheckout(ctx context.Context, moduleName, path string) error {
	replacement, ok := h.replacements[moduleName]
	if !ok || !replacement.IsGit() || replacement.Commit == "" {
		return nil
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return nil
	}
	src, err := gitsource.Parse(replacement.Source)
	if err != nil {
		return NewDependencyLoadError(path, err)
	}
	_, err = h.gitCheckout(ctx, src, replacement.Commit, moduleName)
	return err
}
