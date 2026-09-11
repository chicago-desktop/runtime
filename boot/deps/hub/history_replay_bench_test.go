package hub

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func BenchmarkDependencySelection(b *testing.B) {
	client := &fakeHub{getManifest: func(context.Context, string, string, string) (*ModuleManifest, error) {
		return &ModuleManifest{Org: "acme", Name: "worker", Version: "1.0.0", VersionID: "1.0.0", Digest: "sha256:" + strings.Repeat("a", 64)}, nil
	}}
	handler, err := NewDependencyHandler(DependencyHandlerOptions{Hub: client, Logger: zap.NewNop(), VendorDir: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	deps := []DependencyDefinition{{Component: "acme/worker", Version: "1.0.0"}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := handler.resolveEffectiveModules(b.Context(), deps, nil, nil); err != nil {
			b.Fatal(err)
		}
	}
}
