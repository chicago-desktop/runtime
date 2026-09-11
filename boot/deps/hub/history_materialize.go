package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	ctxapi "github.com/wippyai/runtime/api/context"
	"github.com/wippyai/runtime/api/payload"
	regapi "github.com/wippyai/runtime/api/registry"
	entryencoding "github.com/wippyai/runtime/api/registry/history/encoding"
	"github.com/wippyai/runtime/api/registry/history/migration"
	historyv1 "github.com/wippyai/runtime/api/registry/history/v1"
	syspayload "github.com/wippyai/runtime/system/payload"
	jsonpayload "github.com/wippyai/runtime/system/payload/json"
	msgpackpayload "github.com/wippyai/runtime/system/payload/msgpack"
	yamlpayload "github.com/wippyai/runtime/system/payload/yaml"
	"github.com/wippyai/runtime/system/registry/expansion"
	"github.com/wippyai/runtime/system/registry/history/postgres"
	"github.com/wippyai/runtime/system/registry/topology"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type MaterializeHistoryOptions struct {
	Hub                HubClient
	ScratchDir         string
	MaximumRecordBytes int
}

func MaterializeHistory(ctx context.Context, source io.Reader, destination io.Writer, options MaterializeHistoryOptions) error {
	reader, err := migration.NewReader(source, options.MaximumRecordBytes)
	if err != nil {
		return err
	}
	header := reader.Header()
	if header.Format != migration.RawFormat {
		return errors.New("materialization requires a raw source bundle")
	}
	sum := sha256.Sum256(header.Baseline)
	if !bytes.Equal(sum[:], header.BaselineDigest) {
		return errors.New("baseline digest does not match the source bundle")
	}
	baseline := new(historyv1.Version)
	if err := proto.Unmarshal(header.Baseline, baseline); err != nil {
		return err
	}
	if baseline.Revision != 0 || baseline.ParentRevision != 0 {
		return errors.New("baseline must be root zero")
	}
	baselineState, _, err := decodeMaterializedState(baseline)
	if err != nil {
		return err
	}
	scratch, err := os.MkdirTemp(options.ScratchDir, "history-replay-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	resolver, err := topology.NewDefaultResolver()
	if err != nil {
		return err
	}
	handler, err := NewDependencyHandler(DependencyHandlerOptions{Hub: options.Hub, Resolver: resolver, LockPath: filepath.Join(scratch, "wippy.lock"), VendorDir: filepath.Join(scratch, "vendor"), ArtifactRoot: filepath.Join(scratch, "artifacts")})
	if err != nil {
		return err
	}
	handler.lock = nil
	transcoder := syspayload.NewTranscoder()
	jsonpayload.Register(transcoder)
	yamlpayload.Register(transcoder)
	msgpackpayload.Register(transcoder)
	ctx = ctxapi.WithAppContext(ctx, ctxapi.NewAppContext())
	ctx = payload.WithTranscoder(ctx, transcoder)
	ctx = regapi.WithDependencyAccess(ctx, regapi.DependencyAccessOnline)
	header.Format = migration.MaterializedFormat
	writer, err := migration.NewWriter(destination, header, options.MaximumRecordBytes)
	if err != nil {
		return err
	}
	decoder := postgres.NewLegacyDecoder(baselineState)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		record, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return writer.Close()
		}
		if err != nil {
			return err
		}
		snapshot, err := materializeRecord(ctx, handler, resolver, transcoder, decoder, baseline, record, scratch, options.MaximumRecordBytes)
		if err != nil {
			return fmt.Errorf("materialize revision %d: %w", record.Revision, err)
		}
		if proto.Size(snapshot) > options.MaximumRecordBytes {
			return fmt.Errorf("snapshot revision %d exceeds record limit", record.Revision)
		}
		record.Snapshot, err = proto.MarshalOptions{Deterministic: true}.Marshal(snapshot)
		if err != nil {
			return err
		}
		if err := writer.Write(record); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(scratch, strconv.FormatUint(record.Revision, 10)), record.Snapshot, 0600); err != nil {
			return err
		}
	}
}

func materializeRecord(ctx context.Context, handler *DependencyHandler, resolver regapi.DependencyResolver, transcoder payload.Transcoder, decoder *postgres.LegacyDecoder, baseline *historyv1.Version, record *migration.Record, scratch string, maximumBytes int) (*historyv1.Version, error) {
	parent := baseline
	if record.Revision != 0 {
		file, err := os.Open(filepath.Join(scratch, strconv.FormatUint(record.Parent, 10)))
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(file, int64(maximumBytes)+1))
		closeErr := file.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return nil, err
		}
		if len(data) > maximumBytes {
			return nil, errors.New("parent snapshot exceeds record limit")
		}
		parent = new(historyv1.Version)
		if err := proto.Unmarshal(data, parent); err != nil {
			return nil, err
		}
	}
	state, deleted, err := decodeMaterializedState(parent)
	if err != nil {
		return nil, err
	}
	previous, err := decodeRecordedResolution(parent.Resolution, nil)
	if err != nil {
		return nil, err
	}
	resolutionBytes := record.Resolution
	if record.Revision == 0 && len(resolutionBytes) == 0 {
		resolutionBytes = baseline.Resolution
	}
	target, err := decodeRecordedResolution(resolutionBytes, record.ResolutionDigest)
	if err != nil {
		return nil, err
	}
	if target != nil {
		handler.deployment = target.Deployment.Canonical()
	} else {
		handler.deployment = nil
	}
	snapshot := &historyv1.Version{Revision: record.Revision, ParentRevision: record.Parent, Resolution: resolutionBytes, LegacyChangeset: record.Changes}
	var changes regapi.ChangeSet
	if len(record.Changes) != 0 {
		changes, err = decoder.Decode(record.Changes)
		if err != nil {
			return nil, err
		}
	}
	if record.Revision == 0 {
		snapshot.LegacyChangeset = nil
		if len(changes) != 0 {
			return nil, errors.New("root zero contains changes")
		}
		if err := ValidateDependencyResolution(ctx, state, target, transcoder); err != nil {
			return nil, err
		}
		previous = nil
		for _, entry := range state {
			if isRootDependency(entry) {
				changes = append(changes, regapi.Operation{Kind: regapi.EntryUpdate, Entry: entry})
			}
		}
	}
	directive := expansion.NewDependencyDirective(func(ctx context.Context, operation regapi.Operation, state regapi.State) (regapi.DirectiveResult, error) {
		return handler.ExpandRecordedChanges(ctx, regapi.ChangeSet{operation}, state, previous, target)
	}).WithChangesExpansion(func(ctx context.Context, changes regapi.ChangeSet, state regapi.State) (regapi.DirectiveResult, error) {
		return handler.ExpandRecordedChanges(ctx, changes, state, previous, target)
	})
	planner := expansion.NewPlanner(map[regapi.Kind][]regapi.Directive{regapi.NamespaceDependency: {directive}}, resolver, zap.NewNop())
	plan, err := planner.Expand(ctx, changes, state)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			planner.RollbackEffects(ctx, plan.Effects)
		}
	}()
	plan.Ops, err = planner.SortOps(state, plan.Ops)
	if err != nil {
		return nil, err
	}
	values := make(topology.StateMap, len(state)+len(plan.Ops))
	for _, entry := range state {
		values[entry.ID] = entry
	}
	builder := topology.NewStateBuilder(zap.NewNop(), resolver)
	for _, scoped := range plan.Ops {
		operation := scoped.Operation
		if err := builder.ValidateOperation(values, operation); err != nil {
			return nil, err
		}
		if operation.Kind == regapi.EntryDelete {
			delete(values, operation.Entry.ID)
			deleted[operation.Entry.ID] = struct{}{}
		} else {
			values[operation.Entry.ID] = operation.Entry
			delete(deleted, operation.Entry.ID)
		}
	}
	state = make(regapi.State, 0, len(values))
	for _, entry := range values {
		state = append(state, entry)
	}
	if err := ValidateDependencyResolution(ctx, state, target, transcoder); err != nil {
		return nil, err
	}
	if _, err := topology.SortEntriesByDependency(state, resolver); err != nil {
		return nil, err
	}
	snapshot.Entries = make([]*historyv1.Mutation, 0, len(values)+len(deleted))
	for _, entry := range state {
		data, err := entryencoding.EncodeEntry(entry)
		if err != nil {
			return nil, err
		}
		snapshot.Entries = append(snapshot.Entries, &historyv1.Mutation{EntryId: entry.ID.String(), Value: data})
	}
	for id := range deleted {
		snapshot.Entries = append(snapshot.Entries, &historyv1.Mutation{EntryId: id.String(), Deleted: true})
	}
	sort.Slice(snapshot.Entries, func(i, j int) bool { return snapshot.Entries[i].EntryId < snapshot.Entries[j].EntryId })
	if _, err := planner.PrepareEffects(ctx, plan.Effects); err != nil {
		return nil, err
	}
	if err := planner.CommitEffects(ctx, plan.Effects); err != nil {
		return nil, err
	}
	if err := planner.FinalizeEffects(ctx, plan.Effects); err != nil {
		return nil, err
	}
	committed = true
	return snapshot, nil
}

func decodeRecordedResolution(data []byte, digest *string) (*regapi.DependencyResolution, error) {
	if len(data) == 0 {
		if digest != nil {
			return nil, regapi.ErrInvalidDependencyResolution
		}
		return nil, nil
	}
	var resolution regapi.DependencyResolution
	if err := json.Unmarshal(data, &resolution); err != nil {
		return nil, err
	}
	if !resolution.Valid() || digest != nil && resolution.Digest != *digest {
		return nil, regapi.ErrInvalidDependencyResolution
	}
	return resolution.Canonical(), nil
}

func decodeMaterializedState(snapshot *historyv1.Version) (regapi.State, map[regapi.ID]struct{}, error) {
	state := make(regapi.State, 0, len(snapshot.Entries))
	deleted := make(map[regapi.ID]struct{})
	seen := make(map[string]struct{}, len(snapshot.Entries))
	for _, mutation := range snapshot.Entries {
		if mutation == nil {
			return nil, nil, errors.New("snapshot contains a nil entry")
		}
		id := regapi.ParseID(mutation.EntryId).Canonical()
		if id.Name == "" || id.String() != mutation.EntryId {
			return nil, nil, errors.New("snapshot entry ID is not canonical")
		}
		if _, exists := seen[mutation.EntryId]; exists {
			return nil, nil, errors.New("snapshot contains a duplicate entry")
		}
		seen[mutation.EntryId] = struct{}{}
		if mutation.Deleted {
			if len(mutation.Value) != 0 {
				return nil, nil, errors.New("deleted snapshot entry contains a value")
			}
			deleted[id] = struct{}{}
			continue
		}
		entry, err := entryencoding.DecodeEntry(mutation.Value)
		if err != nil {
			return nil, nil, err
		}
		if entry.ID != id {
			return nil, nil, errors.New("snapshot entry ID does not match its value")
		}
		state = append(state, entry)
	}
	return state, deleted, nil
}
