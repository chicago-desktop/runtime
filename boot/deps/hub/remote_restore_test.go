package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	moduleapi "github.com/wippyai/runtime/api/modules"
	regapi "github.com/wippyai/runtime/api/registry"
	historyv1 "github.com/wippyai/runtime/api/registry/history/v1"
	"github.com/wippyai/runtime/system/registry/history/remote"
	"github.com/wippyai/wapp"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type graphHistoryServer struct {
	historyv1.UnimplementedHistoryServiceServer
	graph []byte
}

func (s *graphHistoryServer) GetVersion(context.Context, *historyv1.GetRequest) (*historyv1.Version, error) {
	return &historyv1.Version{Revision: 1, Resolution: s.graph}, nil
}

func TestRemoteHistoryRestoresExactDeploymentWithoutLock(t *testing.T) {
	t.Chdir(t.TempDir())
	artifact := buildWappBytes(t, []wapp.Entry{{ID: wapp.NewID("acme.worker", "service"), Kind: regapi.EntryKind}})
	sum := sha256.Sum256(artifact)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	selected := regapi.ResolvedModule{Name: "acme/worker", Version: "1.2.3", Source: moduleSourceHub, Digest: digest, SizeBytes: uint64(len(artifact))}
	resolution := (&regapi.DependencyResolution{Modules: []regapi.ResolvedModule{selected}, Deployment: &regapi.Deployment{Root: selected.Name, Modules: []regapi.ResolvedModule{selected}}}).Canonical()
	graph, err := json.Marshal(resolution)
	require.NoError(t, err)
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	historyv1.RegisterHistoryServiceServer(server, &graphHistoryServer{graph: graph})
	go server.Serve(listener)
	defer server.Stop()
	connection, err := grpc.NewClient("passthrough:///history", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	require.NoError(t, err)
	defer connection.Close()
	history, err := remote.New(connection, remote.Config{Key: &historyv1.RegistryKey{TenantId: "test", EnvironmentId: "test", RegistryId: "test"}, ReplicaID: "test", Timeout: time.Second, PollInterval: time.Millisecond})
	require.NoError(t, err)
	downloads := 0
	client := &fakeHub{
		getManifest: func(context.Context, string, string, string) (*ModuleManifest, error) {
			t.Fatal("restore requested a new dependency selection")
			return nil, nil
		},
		getDownload: func(_ context.Context, params *DownloadParams) (*DownloadInfo, error) {
			require.Equal(t, "1.2.3", params.Version)
			return &DownloadInfo{URL: "memory://worker", Digest: digest, Size: uint64(len(artifact))}, nil
		},
		downloadFile: func(_ context.Context, _ string, destination string) error {
			downloads++
			return os.WriteFile(destination, artifact, 0o600)
		},
	}
	handler, err := NewDependencyHandler(DependencyHandlerOptions{Hub: client, Logger: zap.NewNop(), VendorDir: t.TempDir()})
	require.NoError(t, err)
	ctx := moduleapi.WithSourceRegistry(newTestContext(), moduleapi.NewSourceRegistry())
	require.NoError(t, handler.PrepareRestore(ctx, history))
	require.Equal(t, 1, downloads)
	sources := moduleapi.GetSourceRegistry(ctx).Snapshot()
	require.Len(t, sources, 1)
	require.True(t, sources[selected.Name].DeploymentRoot)
	require.Equal(t, resolution.Deployment, handler.deployment)
	_, err = os.Stat("wippy.lock")
	require.True(t, os.IsNotExist(err))
}
