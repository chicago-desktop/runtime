// SPDX-License-Identifier: MPL-2.0

package lock

// File represents the structure of a wippy.lock file.
type File struct {
	Directories  Directories   `yaml:"directories"`
	Modules      []Module      `yaml:"modules,omitempty"`
	Replacements []Replacement `yaml:"replacements,omitempty"`
	Options      Options       `yaml:"options,omitempty"`
}

// Options specifies runtime behavior for module loading.
type Options struct {
	UnpackModules bool `yaml:"unpack_modules,omitempty"` // Extract .wapp to directories (default: false)
}

// Directories specifies paths for module storage and source scanning.
type Directories struct {
	Modules string `yaml:"modules"` // Base directory for vendor storage (e.g., .wippy)
	Src     string `yaml:"src"`     // Source directory to scan for dependencies (e.g., .)
}

// Module represents a locked dependency.
type Module struct {
	Name      string `yaml:"name"`                 // Module identifier in org/module format
	Version   string `yaml:"version"`              // Semantic version (e.g., v0.0.11)
	Hash      string `yaml:"hash,omitempty"`       // Manifest digest (e.g. sha256:...), populated on install
	Source    string `yaml:"source,omitempty"`     // Git repository the module comes from, as written; empty for the Hub
	Commit    string `yaml:"commit,omitempty"`     // Commit the git source resolved to; required with Source
	LocalHash string `yaml:"local_hash,omitempty"` // Tree digest of a git checkout, verified before loading
	Root      bool   `yaml:"root,omitempty"`       // Selected deployment root, not a transitive module
}

// IsGit reports whether the module is taken from a git repository.
func (m Module) IsGit() bool {
	return m.Source != ""
}

// Replacement represents a local module override for development.
type Replacement struct {
	From string `yaml:"from"` // Module name to replace
	To   string `yaml:"to"`   // Local filesystem path (relative to lock file)
	// Source is a git spelling (url#ref) written in place of a directory. It
	// is never persisted; To is bound to the checkout of the commit the lock
	// records for From, or stays empty until wippy update resolves the ref.
	Source string `yaml:"-"`
	// Commit is the commit Source's ref resolved to, once known.
	Commit string `yaml:"-"`
}

// IsGit reports whether the replacement names a git repository.
func (r Replacement) IsGit() bool {
	return r.Source != ""
}

// Changes represents the differences between two lock files.
type Changes struct {
	Installed []Module       // Newly added modules
	Updated   []ModuleChange // Modules with version/hash changes
	Removed   []Module       // Removed modules
}

// ModuleChange represents a module that changed between lock files.
type ModuleChange struct {
	Name       string // Module name
	OldVersion string // Previous version
	NewVersion string // New version
	OldHash    string // Previous hash
	NewHash    string // New hash
}
