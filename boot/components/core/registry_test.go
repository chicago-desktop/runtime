// SPDX-License-Identifier: MPL-2.0

package core

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/boot"
	regapi "github.com/wippyai/runtime/api/registry"
	historyv1 "github.com/wippyai/runtime/api/registry/history/v1"
	bootpkg "github.com/wippyai/runtime/boot"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

func TestReadKindSlice_InvalidTypeDoesNotOverrideDefaults(t *testing.T) {
	cfg := boot.NewConfig(boot.WithSection(RegistryName, map[string]any{
		RegistryDispatchInternalKinds: 42,
	}))

	kinds, ok := readKindSlice(cfg.Sub(RegistryName), RegistryDispatchInternalKinds)
	assert.False(t, ok)
	assert.Nil(t, kinds)
}

func TestReadKindSlice_ValidList(t *testing.T) {
	cfg := boot.NewConfig(boot.WithSection(RegistryName, map[string]any{
		RegistryDispatchInternalKinds: []string{"registry.entry", "ns.dependency"},
	}))

	kinds, ok := readKindSlice(cfg.Sub(RegistryName), RegistryDispatchInternalKinds)
	assert.True(t, ok)
	assert.Equal(t, []string{"registry.entry", "ns.dependency"}, kinds)
}

func TestReadKindSlice_MixedAnyValues(t *testing.T) {
	cfg := boot.NewConfig(boot.WithSection(RegistryName, map[string]any{
		RegistryDispatchInternalKinds: []any{"registry.entry", 7, "ns.definition"},
	}))

	kinds, ok := readKindSlice(cfg.Sub(RegistryName), RegistryDispatchInternalKinds)
	assert.True(t, ok)
	assert.Equal(t, []string{"registry.entry", "ns.definition"}, kinds)
}

func TestCoreDependencyPatternsIncludeExplicitMetadataAndLifecycleRefs(t *testing.T) {
	patterns := append(getDefaultDependencyPatterns(), getLifecycleDependencyPatterns()...)
	paths := make(map[string]bool, len(patterns))
	for _, pattern := range patterns {
		paths[pattern.Path] = pattern.AllowWildcard
	}

	require.Contains(t, paths, "meta.depends_on")
	require.True(t, paths["meta.depends_on"])
	require.Contains(t, paths, "data.*.depends_on")
	require.True(t, paths["data.*.depends_on"])
	require.Contains(t, paths, "data.lifecycle.requires")
	require.True(t, paths["data.lifecycle.requires"])
	require.Contains(t, paths, "data.lifecycle.depends_on")
	require.True(t, paths["data.lifecycle.depends_on"])
}

func TestRegistryPostgresHistoryRequiresDSN(t *testing.T) {
	cfg := boot.NewConfig(boot.WithSection(RegistryName, map[string]any{
		RegistryEnableHistory: true,
		RegistryHistoryType:   "postgres",
	}))
	ctx, err := bootpkg.NewBootstrapContext(zap.NewNop(), cfg)
	require.NoError(t, err)

	loader, err := bootpkg.NewLoader(Artifacts(), Registry())
	require.NoError(t, err)

	_, err = loader.Load(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "history DSN is required")
}

func TestRegistryRemoteHistoryRequiresEndpoint(t *testing.T) {
	cfg := boot.NewConfig(boot.WithSection(RegistryName, map[string]any{RegistryEnableHistory: true, RegistryHistoryType: "grpc"}))
	ctx, err := bootpkg.NewBootstrapContext(zap.NewNop(), cfg)
	require.NoError(t, err)
	loader, err := bootpkg.NewLoader(Artifacts(), Registry())
	require.NoError(t, err)
	_, err = loader.Load(ctx)
	require.ErrorContains(t, err, "history endpoint is required")
}

type emptyRemoteRegistryServer struct {
	historyv1.UnimplementedHistoryServiceServer
}

func (*emptyRemoteRegistryServer) GetVersion(_ context.Context, request *historyv1.GetRequest) (*historyv1.Version, error) {
	if request.Exact {
		return nil, status.Error(codes.NotFound, "history was not found")
	}
	return &historyv1.Version{}, nil
}

func TestRegistryRemoteFirstBoot(t *testing.T) {
	t.Chdir(t.TempDir())
	certificateServer := httptest.NewTLSServer(nil)
	certificate := certificateServer.TLS.Certificates[0]
	certificateServer.Close()
	listenConfig := net.ListenConfig{}
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}})))
	historyv1.RegisterHistoryServiceServer(server, &emptyRemoteRegistryServer{})
	go server.Serve(listener)
	defer server.Stop()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0600))
	require.NoError(t, os.WriteFile(tokenFile, []byte("test-token"), 0600))
	cfg := boot.NewConfig(boot.WithSection(RegistryName, map[string]any{
		RegistryEnableHistory: true, RegistryHistoryType: "grpc",
		"history_endpoint": listener.Addr().String(), "history_token_file": tokenFile, "history_ca_file": caFile,
		"history_tenant_id": "test", "history_environment_id": "test", "history_registry_id": "test", "history_replica_id": "test",
		"history_timeout": time.Second, "history_poll_interval": time.Millisecond,
	}))
	ctx, err := bootpkg.NewBootstrapContext(zap.NewNop(), cfg)
	require.NoError(t, err)
	component := Registry()
	loader, err := bootpkg.NewLoader(Artifacts(), component)
	require.NoError(t, err)
	ctx, err = loader.Load(ctx)
	require.NoError(t, err)
	defer loader.Shutdown(ctx)
	history := regapi.GetRegistry(ctx).History()
	head, err := history.Head()
	require.NoError(t, err)
	require.Zero(t, head.ID())
	require.NoError(t, regapi.GetRegistry(ctx).LoadState(ctx, nil, head))
	require.NoError(t, loader.Start(ctx))
}
