package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wippyai/runtime/api/registry"
	entryencoding "github.com/wippyai/runtime/api/registry/history/encoding"
	historyv1 "github.com/wippyai/runtime/api/registry/history/v1"
	"github.com/wippyai/runtime/internal/version"
	legacy "github.com/wippyai/runtime/system/registry/history/postgres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const MaxMessageBytes = entryencoding.MaxEntryBytes

var ErrCommitUnknown = errors.New("history commit status is unknown; retry the same change")

type Config struct {
	Key             *historyv1.RegistryKey
	ReplicaID       string
	Timeout         time.Duration
	PollInterval    time.Duration
	MaxMessageBytes int
}

type History struct {
	key             *historyv1.RegistryKey
	pending         *historyv1.SubmitRequest
	pendingRestore  *historyv1.RestoreRequest
	pendingReport   *historyv1.AppliedRequest
	legacyDecoder   *legacy.LegacyDecoder
	closer          io.Closer
	client          historyv1.HistoryServiceClient
	replicaID       string
	actor           string
	causal          []*historyv1.Dot
	timeout         time.Duration
	pollInterval    time.Duration
	maxMessageBytes int
	counter         uint64
	applied         uint64
	reported        uint64
	mu              sync.Mutex
	reportMu        sync.Mutex
	legacyMu        sync.Mutex
	actorReady      bool
}

var _ registry.PublishedHistory = (*History)(nil)

func New(connection grpc.ClientConnInterface, cfg Config) (*History, error) {
	if connection == nil || cfg.Key.GetTenantId() == "" || cfg.Key.GetEnvironmentId() == "" || cfg.Key.GetRegistryId() == "" || cfg.ReplicaID == "" {
		return nil, errors.New("history connection, registry identity, and replica ID are required")
	}
	if cfg.Timeout <= 0 || cfg.PollInterval <= 0 {
		return nil, errors.New("history timeout and poll interval must be positive")
	}
	if cfg.MaxMessageBytes == 0 {
		cfg.MaxMessageBytes = MaxMessageBytes
	}
	if cfg.MaxMessageBytes < 0 {
		return nil, errors.New("history message size must be positive")
	}
	return &History{client: historyv1.NewHistoryServiceClient(connection), key: proto.Clone(cfg.Key).(*historyv1.RegistryKey), replicaID: cfg.ReplicaID, actor: cfg.ReplicaID, timeout: cfg.Timeout, pollInterval: cfg.PollInterval, maxMessageBytes: cfg.MaxMessageBytes}, nil
}

func (h *History) SubmitChanges(ctx context.Context, changes registry.ChangeSet, resolution *registry.DependencyResolution) (*registry.HistoryReceipt, error) {
	mutations := make([]*historyv1.Mutation, 0, len(changes))
	positions := make(map[string]int, len(changes))
	for _, op := range changes {
		id := op.Entry.ID.Canonical()
		if id.Name == "" {
			return nil, errors.New("entry ID is required")
		}
		name := id.String()
		mutation := &historyv1.Mutation{EntryId: name}
		switch op.Kind {
		case registry.EntryCreate, registry.EntryUpdate:
			data, err := entryencoding.EncodeEntry(op.Entry)
			if err != nil {
				return nil, err
			}
			mutation.Value = data
		case registry.EntryDelete:
			mutation.Deleted = true
		default:
			return nil, errors.New("unsupported registry operation")
		}
		if index, exists := positions[name]; exists {
			mutations[index] = mutation
		} else {
			positions[name] = len(mutations)
			mutations = append(mutations, mutation)
		}
	}
	var graph []byte
	if resolution != nil {
		if !resolution.Valid() {
			return nil, registry.ErrInvalidDependencyResolution
		}
		var err error
		graph, err = json.Marshal(resolution.Canonical())
		if err != nil {
			return nil, err
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pendingRestore != nil {
		return nil, fmt.Errorf("%w: %s", ErrCommitUnknown, h.pendingRestore.RequestId)
	}
	req := &historyv1.SubmitRequest{Mutations: mutations, Resolution: graph}
	if h.pending != nil {
		previous := &historyv1.SubmitRequest{Mutations: h.pending.Mutations, Resolution: h.pending.Resolution}
		if !proto.Equal(previous, req) {
			return nil, fmt.Errorf("%w: %s", ErrCommitUnknown, h.pending.RequestId)
		}
		req = h.pending
	} else {
		if err := h.initializeActor(ctx); err != nil {
			return nil, err
		}
		req.Key = h.key
		req.RequestId = uuid.NewString()
		req.Dot = &historyv1.Dot{Actor: h.actor, Counter: h.counter + 1}
		req.Context = h.requestContext()
		if proto.Size(req) > h.maxMessageBytes {
			return nil, errors.New("changes exceed the message size limit")
		}
		h.pending = req
	}
	callCtx, cancel := context.WithTimeout(ctx, h.timeout)
	result, err := h.client.Submit(callCtx, req, grpc.MaxCallSendMsgSize(h.maxMessageBytes))
	cancel()
	if err != nil {
		if !retryable(err) {
			h.pending = nil
			return nil, err
		}
		receiptCtx, receiptCancel := context.WithTimeout(context.WithoutCancel(ctx), h.timeout)
		receipt, receiptErr := h.client.GetReceipt(receiptCtx, &historyv1.GetReceiptRequest{Key: h.key, RequestId: req.RequestId})
		receiptCancel()
		if receiptErr != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrCommitUnknown, req.RequestId, errors.Join(err, receiptErr))
		}
		h.counter = req.Dot.Counter
		h.pending = nil
		return convertReceipt(receipt), nil
	}
	if result.GetReceipt() == nil {
		return nil, fmt.Errorf("%w: %s: empty receipt", ErrCommitUnknown, req.RequestId)
	}
	h.counter = req.Dot.Counter
	h.pending = nil
	return convertReceipt(result.Receipt), nil
}

func (h *History) AwaitPublished(ctx context.Context, receipt *registry.HistoryReceipt) (*registry.PublishedState, error) {
	if receipt == nil || receipt.RequestID == "" {
		return nil, errors.New("history receipt is required")
	}
	current := *receipt
	delay := h.pollInterval
	for {
		switch current.Status {
		case "published":
			if current.PublishedRevision == 0 {
				return nil, errors.New("published receipt has no version")
			}
			return h.ReadPublished(ctx, current.PublishedRevision)
		case "conflicted":
			return nil, fmt.Errorf("%w: %s: %s", registry.ErrHistoryConflict, current.RequestID, current.Message)
		case "rejected":
			return nil, fmt.Errorf("%w: %s: %s", registry.ErrHistoryRejected, current.RequestID, current.Message)
		case "stored", "superseded":
		default:
			return nil, fmt.Errorf("invalid history receipt status: %s", current.Status)
		}
		if err := wait(ctx, delay); err != nil {
			return nil, fmt.Errorf("wait for history request %s: %w", current.RequestID, err)
		}
		callCtx, cancel := context.WithTimeout(ctx, h.timeout)
		result, err := h.client.GetReceipt(callCtx, &historyv1.GetReceiptRequest{Key: h.key, RequestId: receipt.RequestID})
		cancel()
		if err != nil {
			if !retryable(err) {
				return nil, err
			}
		} else {
			current = *convertReceipt(result)
		}
	}
}

func (h *History) ReadPublished(ctx context.Context, revision uint64) (*registry.PublishedState, error) {
	raw, err := h.readVersion(ctx, revision, false)
	if err != nil {
		return nil, err
	}
	return decodeVersion(raw)
}

func (h *History) RestoreChanges(ctx context.Context, revision uint64) (*registry.HistoryReceipt, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending != nil {
		return nil, fmt.Errorf("%w: %s", ErrCommitUnknown, h.pending.RequestId)
	}
	request := h.pendingRestore
	if request != nil {
		if request.TargetRevision != revision {
			return nil, fmt.Errorf("%w: %s", ErrCommitUnknown, request.RequestId)
		}
	} else {
		if err := h.initializeActor(ctx); err != nil {
			return nil, err
		}
		request = &historyv1.RestoreRequest{Key: h.key, RequestId: uuid.NewString(), Dot: &historyv1.Dot{Actor: h.actor, Counter: h.counter + 1}, Context: h.requestContext(), TargetRevision: revision}
		h.pendingRestore = request
	}
	callCtx, cancel := context.WithTimeout(ctx, h.timeout)
	result, err := h.client.Restore(callCtx, request)
	cancel()
	if err != nil {
		if !retryable(err) {
			h.pendingRestore = nil
			return nil, err
		}
		receiptCtx, receiptCancel := context.WithTimeout(context.WithoutCancel(ctx), h.timeout)
		receipt, receiptErr := h.client.GetReceipt(receiptCtx, &historyv1.GetReceiptRequest{Key: h.key, RequestId: request.RequestId})
		receiptCancel()
		if receiptErr != nil {
			return nil, fmt.Errorf("%w: restore request %s: %w", ErrCommitUnknown, request.RequestId, errors.Join(err, receiptErr))
		}
		h.counter = request.Dot.Counter
		h.pendingRestore = nil
		return convertReceipt(receipt), nil
	}
	if result.GetReceipt() == nil {
		return nil, fmt.Errorf("%w: %s: empty restore receipt", ErrCommitUnknown, request.RequestId)
	}
	h.counter = request.Dot.Counter
	h.pendingRestore = nil
	return convertReceipt(result.GetReceipt()), nil
}

func (h *History) initializeActor(ctx context.Context) error {
	if !h.actorReady {
		callCtx, cancel := context.WithTimeout(ctx, h.timeout)
		candidate, err := h.client.GetCandidate(callCtx, &historyv1.GetRequest{Key: h.key}, grpc.MaxCallRecvMsgSize(h.maxMessageBytes))
		cancel()
		if err != nil && status.Code(err) != codes.NotFound {
			return err
		}
		if err == nil && candidate == nil {
			return errors.New("empty history candidate response")
		}
		for _, dot := range candidate.GetContext() {
			if dot.GetActor() == h.actor {
				h.counter = max(h.counter, dot.GetCounter())
			}
		}
		h.actorReady = true
	}
	if h.counter >= math.MaxInt64 {
		return errors.New("history replica counter is exhausted")
	}
	return nil
}

func (h *History) requestContext() []*historyv1.Dot {
	if h.counter == 0 {
		return h.causal
	}
	for _, dot := range h.causal {
		if dot.Actor == h.actor && dot.Counter == h.counter {
			return h.causal
		}
	}
	result := make([]*historyv1.Dot, 0, len(h.causal)+1)
	for _, dot := range h.causal {
		if dot.Actor != h.actor {
			result = append(result, dot)
		}
	}
	return append(result, &historyv1.Dot{Actor: h.actor, Counter: h.counter})
}

func (h *History) FollowPublished(ctx context.Context, after uint64, apply func(*registry.PublishedState) error) error {
	if apply == nil {
		return errors.New("publication callback is required")
	}
	delay := backoff.DefaultConfig.BaseDelay
	for {
		if err := h.flushApplied(ctx); err != nil {
			if !retryable(err) {
				return err
			}
			if err := wait(ctx, delay); err != nil {
				return err
			}
			delay = min(time.Duration(float64(delay)*backoff.DefaultConfig.Multiplier), backoff.DefaultConfig.MaxDelay)
			continue
		}
		stream, err := h.client.Watch(ctx, &historyv1.ReadRequest{Key: h.key, AfterRevision: after, Limit: 0}, grpc.MaxCallRecvMsgSize(h.maxMessageBytes))
		if err == nil {
			for {
				next, receiveErr := stream.Recv()
				if receiveErr != nil {
					err = receiveErr
					break
				}
				if next.Revision <= after {
					continue
				}
				published, decodeErr := decodeVersion(next)
				if decodeErr != nil {
					return decodeErr
				}
				if applyErr := apply(published); applyErr != nil {
					return applyErr
				}
				after = next.Revision
				delay = backoff.DefaultConfig.BaseDelay
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retryable(err) && !errors.Is(err, io.EOF) {
			return err
		}
		if err := wait(ctx, delay); err != nil {
			return err
		}
		delay = min(time.Duration(float64(delay)*backoff.DefaultConfig.Multiplier), backoff.DefaultConfig.MaxDelay)
	}
}

func (h *History) ReportApplied(ctx context.Context, published *registry.PublishedState, applicationErr error) error {
	if published == nil || published.Version == nil {
		return errors.New("published version is required")
	}
	revision := uint64(published.Version.ID())
	if applicationErr == nil {
		h.mu.Lock()
		if revision >= h.applied {
			h.causal = make([]*historyv1.Dot, len(published.Context))
			for i, dot := range published.Context {
				h.causal[i] = &historyv1.Dot{Actor: dot.Actor, Counter: dot.Counter}
			}
			h.applied = revision
		}
		h.mu.Unlock()
	}
	request := &historyv1.AppliedRequest{Key: h.key, ReplicaId: h.replicaID, Revision: revision}
	if applicationErr != nil {
		request.Error = applicationErr.Error()
	}
	h.reportMu.Lock()
	defer h.reportMu.Unlock()
	if request.Revision < h.reported {
		return h.flushAppliedLocked(ctx)
	}
	if h.pendingReport == nil || request.Revision >= h.pendingReport.Revision {
		h.pendingReport = request
	}
	return h.flushAppliedLocked(ctx)
}

func (h *History) flushApplied(ctx context.Context) error {
	h.reportMu.Lock()
	defer h.reportMu.Unlock()
	return h.flushAppliedLocked(ctx)
}

func (h *History) flushAppliedLocked(ctx context.Context) error {
	if h.pendingReport == nil {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	if _, err := h.client.ReportApplied(callCtx, h.pendingReport); err != nil {
		return err
	}
	h.reported = h.pendingReport.Revision
	h.pendingReport = nil
	return nil
}

func decodeVersion(value *historyv1.Version) (*registry.PublishedState, error) {
	if value == nil || uint64(uint(value.Revision)) != value.Revision {
		return nil, errors.New("invalid published version")
	}
	published := &registry.PublishedState{Version: version.New(uint(value.Revision)), Changes: make(registry.ChangeSet, len(value.Entries)), Context: make([]registry.HistoryDot, len(value.Context))}
	seen := make(map[registry.ID]struct{}, len(value.Entries))
	for i, mutation := range value.Entries {
		if mutation == nil {
			return nil, errors.New("nil published entry")
		}
		id := registry.ParseID(mutation.EntryId).Canonical()
		if id.Name == "" || id.String() != mutation.EntryId {
			return nil, errors.New("invalid published entry ID")
		}
		if _, ok := seen[id]; ok {
			return nil, errors.New("duplicate published entry")
		}
		seen[id] = struct{}{}
		if mutation.Deleted {
			if len(mutation.Value) != 0 {
				return nil, errors.New("deleted entry has a value")
			}
			published.Changes[i] = registry.Operation{Kind: registry.EntryDelete, Entry: registry.Entry{ID: id}}
		} else {
			entry, err := entryencoding.DecodeEntry(mutation.Value)
			if err != nil {
				return nil, err
			}
			if entry.ID != id {
				return nil, errors.New("published entry ID does not match its value")
			}
			published.Changes[i] = registry.Operation{Kind: registry.EntryUpdate, Entry: entry}
		}
	}
	if len(value.Resolution) > 0 {
		if err := json.Unmarshal(value.Resolution, &published.Resolution); err != nil {
			return nil, err
		}
		if !published.Resolution.Valid() {
			return nil, registry.ErrInvalidDependencyResolution
		}
	}
	for i, dot := range value.Context {
		if dot.GetActor() == "" || dot.GetCounter() == 0 {
			return nil, errors.New("invalid published causal context")
		}
		published.Context[i] = registry.HistoryDot{Actor: dot.Actor, Counter: dot.Counter}
	}
	return published, nil
}

func convertReceipt(value *historyv1.Receipt) *registry.HistoryReceipt {
	if value == nil {
		return nil
	}
	return &registry.HistoryReceipt{RequestID: value.RequestId, Revision: value.Revision, PublishedRevision: value.PublishedRevision, Status: value.Status, Message: value.Message}
}

func retryable(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Unknown, codes.Aborted:
		return true
	default:
		return false
	}
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (h *History) Save(registry.Version, registry.ChangeSet, bool) error {
	return registry.ErrHistoryOperationUnsupported
}
func (h *History) SetHead(registry.Version) error { return registry.ErrHistoryOperationUnsupported }
func (h *History) Head() (registry.Version, error) {
	published, err := h.ReadPublished(context.Background(), 0)
	if err != nil {
		return nil, err
	}
	return published.Version, nil
}
func (h *History) MaxVersionID() (uint, error) {
	head, err := h.Head()
	if err != nil {
		return 0, err
	}
	return head.ID(), nil
}
func (h *History) GetDependencyResolution(target registry.Version) (*registry.DependencyResolution, error) {
	if target == nil {
		return nil, errors.New("target version is required")
	}
	raw, err := h.readVersion(context.Background(), uint64(target.ID()), true)
	if err != nil {
		return nil, err
	}
	published, err := decodeVersion(raw)
	if err != nil {
		return nil, err
	}
	if published.Resolution == nil {
		return nil, registry.ErrDependencyResolutionNotFound
	}
	return published.Resolution, nil
}
func (h *History) SaveWithDependencyResolution(registry.Version, registry.ChangeSet, *registry.DependencyResolution, bool) error {
	return registry.ErrHistoryOperationUnsupported
}
func (h *History) CheckpointDependencyResolution(registry.Version, *registry.DependencyResolution) error {
	return registry.ErrHistoryOperationUnsupported
}
