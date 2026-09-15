// SPDX-License-Identifier: MPL-2.0

package hub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

func effectPreviewDigest(kind string, value any) (string, error) {
	encoded, err := json.Marshal(struct {
		Value any    `json:"value"`
		Kind  string `json:"kind"`
	}{Kind: kind, Value: value})
	if err != nil {
		return "", fmt.Errorf("measure %s effect: %w", kind, err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func (e *sourceEffect) previewValue() any {
	if e == nil {
		return nil
	}
	modules := append([]string(nil), e.modules...)
	sort.Strings(modules)
	return struct {
		Desired any      `json:"desired"`
		Modules []string `json:"modules"`
	}{Desired: e.desired, Modules: modules}
}

func (e *sourceEffect) PreviewDigest() (string, error) {
	return effectPreviewDigest("hub.sources", e.previewValue())
}

func (e *moduleFilesystemEffect) PreviewDigest() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	type target struct {
		Module string `json:"module"`
		Path   string `json:"path"`
	}
	targets := make([]target, len(e.staged))
	for i, staged := range e.staged {
		targets[i] = target{Module: staged.module, Path: staged.targetDir}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Module != targets[j].Module {
			return targets[i].Module < targets[j].Module
		}
		return targets[i].Path < targets[j].Path
	})
	return effectPreviewDigest("hub.module_filesystem", struct {
		Sources any      `json:"sources,omitempty"`
		Targets []target `json:"targets"`
	}{Targets: targets, Sources: e.sources.previewValue()})
}

func (e *embedPackEffect) PreviewDigest() (string, error) {
	type pack struct {
		Module  string `json:"module"`
		Version string `json:"version"`
		Path    string `json:"path,omitempty"`
	}
	staged := make([]pack, len(e.staged))
	for i, item := range e.staged {
		staged[i] = pack{Module: item.module, Version: item.version, Path: item.packPath}
	}
	obsolete := make([]pack, len(e.obsolete))
	for i, item := range e.obsolete {
		obsolete[i] = pack{Module: item.module, Version: item.version}
	}
	sort.Slice(staged, func(i, j int) bool {
		if staged[i].Module != staged[j].Module {
			return staged[i].Module < staged[j].Module
		}
		if staged[i].Version != staged[j].Version {
			return staged[i].Version < staged[j].Version
		}
		return staged[i].Path < staged[j].Path
	})
	sort.Slice(obsolete, func(i, j int) bool {
		if obsolete[i].Module != obsolete[j].Module {
			return obsolete[i].Module < obsolete[j].Module
		}
		return obsolete[i].Version < obsolete[j].Version
	})
	return effectPreviewDigest("hub.embed_packs", struct {
		Staged   []pack `json:"staged"`
		Obsolete []pack `json:"obsolete"`
	}{Staged: staged, Obsolete: obsolete})
}
