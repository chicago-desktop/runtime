// SPDX-License-Identifier: MPL-2.0

package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/wippyai/runtime/api/attrs"
	"github.com/wippyai/runtime/api/registry"
)

type previewEntry struct {
	Meta     attrs.Bag
	Data     any
	ID       string
	Kind     string
	Format   string
	Registry registry.EntryMetadata
}

type previewOperation struct {
	Original *previewEntry
	Kind     string
	Entry    previewEntry
}

func measuredEntry(entry registry.Entry) previewEntry {
	meta := entry.Meta
	if meta == nil {
		meta = attrs.Bag{}
	}
	result := previewEntry{ID: entry.ID.String(), Kind: entry.Kind, Meta: meta, Registry: entry.Registry}
	if entry.Data != nil {
		result.Format, result.Data = entry.Data.Format(), entry.Data.Data()
	}
	return result
}

func measuredOperations(changes registry.ChangeSet) []previewOperation {
	result := make([]previewOperation, len(changes))
	for i, op := range changes {
		result[i] = previewOperation{Kind: op.Kind, Entry: measuredEntry(op.Entry)}
		if op.OriginalEntry != nil {
			entry := measuredEntry(*op.OriginalEntry)
			result[i].Original = &entry
		}
	}
	return result
}

func previewDigest(all, history registry.ChangeSet, resolution *registry.DependencyResolution, effects []registry.Effect) (string, error) {
	type measuredEffect struct {
		Kind   string `json:"kind"`
		Digest string `json:"digest"`
	}
	measuredEffects := make([]measuredEffect, len(effects))
	for i, effect := range effects {
		if effect == nil || isNilEffect(effect) {
			return "", fmt.Errorf("measure registry preview effect %d: nil effect", i)
		}
		previewable, ok := effect.(registry.PreviewEffect)
		if !ok {
			return "", fmt.Errorf("measure registry preview effect %d: %T does not expose a preview digest", i, effect)
		}
		digest, err := previewable.PreviewDigest()
		if err != nil {
			return "", fmt.Errorf("measure registry preview effect %d: %w", i, err)
		}
		if !validPreviewEffectDigest(digest) {
			return "", fmt.Errorf("measure registry preview effect %d: invalid preview digest", i)
		}
		measuredEffects[i] = measuredEffect{Kind: reflect.TypeOf(effect).String(), Digest: digest}
	}
	encoded, err := json.Marshal(struct {
		Changes, History []previewOperation
		Resolution       *registry.DependencyResolution
		Effects          []measuredEffect
	}{measuredOperations(all), measuredOperations(history), resolution.Canonical(), measuredEffects})
	if err != nil {
		return "", fmt.Errorf("measure registry preview: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func isNilEffect(effect registry.Effect) bool {
	value := reflect.ValueOf(effect)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func validPreviewEffectDigest(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
