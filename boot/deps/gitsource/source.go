// SPDX-License-Identifier: MPL-2.0

// Package gitsource resolves modules from git repositories: it parses the
// source spellings accepted in ns.dependency components and workspace
// replacements, keeps one bare clone per repository plus one checkout per
// commit under a cache directory, and drives the system git binary. It knows
// nothing about the lock or the resolver; those decide which commit is wanted.
package gitsource

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Source is a parsed git source.
type Source struct {
	// Raw is the value as written, trimmed. The lock records it verbatim so a
	// reader sees what was asked for.
	Raw string
	// URL is what the system git clones: https for a bare host/path spelling,
	// otherwise the spelling itself with any git+ prefix removed.
	URL string
	// Ref is the part after '#': a tag, a branch or a commit. Empty for a
	// dependency, whose versions are the repository's tags.
	Ref string
	// Key is the canonical host/path identity of the repository, without
	// scheme, credentials, a trailing .git or the ref. Two spellings of one
	// repository share a key, and the cache directory is derived from it.
	Key string
}

var (
	scpLike    = regexp.MustCompile(`^([A-Za-z0-9._-]+@)?([A-Za-z0-9.-]+):([^/].*)$`)
	commitHash = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// IsSource reports whether value names a git repository rather than a Hub
// module (org/module) or a directory. A Hub module is exactly two segments
// with no dot in the first; a git source is anything with a scheme, an
// scp-like git@host: spelling, a .git suffix, a '#ref', or a host-like first
// segment (github.com/org/repo).
func IsSource(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.HasPrefix(value, "git+") || strings.Contains(value, "://") || strings.HasPrefix(value, "git@") {
		return true
	}
	body := value
	if i := strings.IndexByte(body, '#'); i >= 0 {
		body = body[:i]
	}
	if strings.HasSuffix(body, ".git") {
		return true
	}
	if filepath.IsAbs(body) || strings.HasPrefix(body, ".") {
		return false
	}
	if scpLike.MatchString(body) {
		return true
	}
	segments := strings.Split(body, "/")
	if len(segments) < 2 {
		return false
	}
	return strings.Contains(segments[0], ".")
}

// IsCommit reports whether ref is a full 40-hex commit id.
func IsCommit(ref string) bool {
	return commitHash.MatchString(strings.ToLower(strings.TrimSpace(ref)))
}

// Parse parses a git source. The ref, when present, follows '#'.
func Parse(value string) (Source, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return Source{}, errors.New("git source is empty")
	}
	body, ref := raw, ""
	if i := strings.LastIndexByte(raw, '#'); i >= 0 {
		body, ref = raw[:i], strings.TrimSpace(raw[i+1:])
		if ref == "" {
			return Source{}, fmt.Errorf("git source %q has an empty ref after '#'", raw)
		}
	}
	body = strings.TrimSpace(strings.TrimPrefix(body, "git+"))
	if body == "" {
		return Source{}, fmt.Errorf("git source %q has no repository", raw)
	}

	src := Source{Raw: raw, Ref: ref}
	switch {
	case strings.Contains(body, "://"):
		parsed, err := url.Parse(body)
		if err != nil {
			return Source{}, fmt.Errorf("git source %q: %w", raw, err)
		}
		if parsed.Host == "" && parsed.Scheme != "file" {
			return Source{}, fmt.Errorf("git source %q has no host", raw)
		}
		src.URL = body
		src.Key = keyFor(parsed.Host, parsed.Path)
	case scpLike.MatchString(body):
		match := scpLike.FindStringSubmatch(body)
		src.URL = body
		src.Key = keyFor(match[2], match[3])
	case filepath.IsAbs(body):
		src.URL = body
		src.Key = keyFor("local", filepath.ToSlash(body))
	default:
		// A bare host/path spelling: github.com/org/repo.
		segments := strings.Split(body, "/")
		if len(segments) < 2 || !strings.Contains(segments[0], ".") {
			return Source{}, fmt.Errorf("git source %q is not a url, host/path or git@host:path", raw)
		}
		src.URL = "https://" + body
		src.Key = keyFor(segments[0], strings.Join(segments[1:], "/"))
	}
	if src.Key == "" || strings.HasSuffix(src.Key, "/") {
		return Source{}, fmt.Errorf("git source %q names no repository path", raw)
	}
	return src, nil
}

// keyFor builds the canonical host/path identity used for the cache layout.
func keyFor(host, repoPath string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	repoPath = strings.Trim(strings.TrimSpace(repoPath), "/")
	repoPath = strings.TrimSuffix(repoPath, ".git")
	repoPath = strings.Trim(repoPath, "/")
	repoPath = path.Clean("/" + repoPath)
	repoPath = strings.TrimPrefix(repoPath, "/")
	if repoPath == "." || repoPath == "" {
		return ""
	}
	if host == "" {
		host = "local"
	}
	return host + "/" + repoPath
}

// String returns the source as written.
func (s Source) String() string {
	return s.Raw
}

// SameRepository reports whether two sources name one repository, ignoring
// spelling and ref.
func (s Source) SameRepository(other Source) bool {
	return s.Key != "" && s.Key == other.Key
}
