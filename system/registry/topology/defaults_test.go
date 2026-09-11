package topology

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/attrs"
	"github.com/wippyai/runtime/api/payload"
	"github.com/wippyai/runtime/api/registry"
)

func TestDefaultResolverFindsMetadataAndLifecycleDependencies(t *testing.T) {
	resolver, err := NewDefaultResolver()
	require.NoError(t, err)
	entry := registry.Entry{ID: registry.NewID("test", "entry"), Meta: attrs.Bag{"parent": "test:parent"}, Data: payload.New(map[string]any{"lifecycle": map[string]any{"requires": []string{"test:required"}}})}
	require.Equal(t, []string{"test:parent", "test:required"}, resolver.Extract(entry))
}
