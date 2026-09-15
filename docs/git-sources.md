# Modules from git

**Status:** contract, 2026-09-15 (revised the same day: a dependency names
the repository, as Composer does with a VCS repository). Owner's decision:
the desktop's modules are taken from their GitHub repositories, not from
the Hub, so the runtime must resolve a dependency from a git repository by
its tags.

## What exists

`boot/deps` knows two sources: the Hub (the registry client in
`boot/deps/client`, versions and hashes in `wippy.lock`) and local
replacements (`workspace.replacements` in `.wippy.yaml`, a directory next
to the application, `lock.Replacement{From, To}`; the module is loaded from
that directory through `config.NewSourceFS` and hashed into `local_hash`).
A dependency is an `ns.dependency` entry: `component: org/module` plus a
`version` range, resolved against the Hub.

## The feature

A dependency may name a git repository instead of a Hub module:

```yaml
- name: shell
  kind: ns.dependency
  version: '>=0.1.0'
  component: github.com/chicago-desktop/shell
```

- **Form of `component`.** A Hub module is `org/module` (two segments, no
  dot in the first). A git source is anything else the system `git` can
  clone: `github.com/org/repo` (https assumed), a full `https://…` or
  `ssh://…` url, `git@host:org/repo.git`. The module's own name is what
  its `wippy.yaml` says (`organization/module`); the lock and every other
  module refer to it by that name, exactly as with a Hub module. Two
  dependencies that resolve to the same module name from different sources
  are a conflict, refused with both sources.
- **Versions are tags.** `version` is the same range syntax as for the Hub
  (`>=0.1.0`, `^0.1`, `*`); the candidates are the repository's tags that
  parse as semantic versions (`v0.1.3` and `0.1.3` alike); the highest
  matching tag wins, exactly as the Hub's resolution picks a version. A
  range that matches no tag is refused naming the tags seen. `version: '*'`
  with no tags at all refuses too: a branch is not a version — pin a
  branch or a commit through a replacement (below) when that is wanted.
- **Transitive.** A module fetched from git may itself declare
  dependencies with git sources; they resolve the same way. A git module
  may depend on Hub modules and vice versa.
- **Replacements** stay what they are — a directory overrides a name for
  development — and gain one form: `chicago/shell: https://github.com/…#ref`
  pins a branch or a commit instead of a tag (the ref is resolved on
  `update` and recorded as a commit).
- **Resolution and the lock.** `wippy update` lists the tags
  (`git ls-remote --tags`), picks the version, resolves it to a commit and
  writes the lock; `wippy install` and the boot use the commit from the
  lock and never look at the tags again. The lock entry:

  ```yaml
  - name: chicago/shell
    version: 0.1.3
    source: github.com/chicago-desktop/shell
    commit: 42c349180724271df4a875998b0c464942da881f
    local_hash: sha256:…     # the tree, as for a directory replacement
  ```

  An entry with `source` and no `commit` is refused at boot, as a
  dependency missing from the lock is.
- **Cache.** `$HOME/.wippy/git/<host>/<path>/` holds one bare clone per
  repository (`repo.git`) and one checkout per commit
  (`checkouts/<commit>/`), read-only for the runtime; overridable with
  `WIPPY_GIT_CACHE`. A commit already checked out needs no network:
  `install` and the boot work offline from the lock.
- **Loading.** The checkout is loaded exactly as a directory replacement:
  the entries come from `src/`; `fs.directory` entries with `base: module`
  resolve against the checkout, so `assets/` need no `embed:`; the
  manifest's `organization/module` is the module's name.
- **Git.** The system `git` binary on PATH; a missing git is one clear
  error naming the source. Authentication is git's (credential helpers,
  the ssh agent, `GIT_*` variables); the runtime adds nothing. Clone
  `--bare` in full (a blobless clone was measured to cost more: the blobs
  are fetched lazily during the checkout, one round trip each — 16 s for
  a two-dozen-file module against 1 s for the whole clone), fetch the one
  commit, materialize the checkout with
  `git --work-tree=<dir> checkout <commit> -- .` (no `.git` inside the
  checkout).
- **Verification.** The tree hash of the checkout is `local_hash`; a
  checkout whose hash differs from the lock is refused, as a changed
  directory replacement is.
- **Out of scope for now.** Sub-directories of a repository (a monorepo),
  signatures, mirrors.

## Tests

Unit tests with a local bare repository made by `git init` in a temp
directory with a few tags — no network: parsing every form of
`component`; picking the highest tag in a range and refusing a range with
no match; a tag with and without the `v`; a transitive git dependency; a
module-name conflict between two sources; the second install offline; the
changed-tree refusal; the lock round trip; a missing `git` binary; a
replacement with `#ref`. The CI's golangci-lint must stay clean
(fieldalignment, noctx, misspell).
