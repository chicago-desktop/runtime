// SPDX-License-Identifier: MPL-2.0

package registry

import (
	"context"
	"errors"
	"testing"

	lua "github.com/wippyai/go-lua"
	regapi "github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/internal/version"
	historymem "github.com/wippyai/runtime/system/registry/history/memory"
	"go.uber.org/zap"
)

type lookupOnlyHistory struct {
	*historymem.Storage
}

func (h *lookupOnlyHistory) Versions() ([]regapi.Version, error) {
	panic("findHistoryVersion enumerated a lookup-capable history")
}

func TestCheckHistoryValid(t *testing.T) {
	l := newTestState()
	defer l.Close()

	history := &History{
		log: zap.NewNop(),
	}

	ud := l.NewUserData()
	ud.Value = history
	l.Push(ud)

	result := checkHistory(l)
	if result == nil {
		t.Error("expected non-nil history")
	}
	if result != history {
		t.Error("expected same history instance")
	}
}

func TestHistoryToString(t *testing.T) {
	l := newTestState()
	defer l.Close()

	historyToString(l)

	result := l.Get(-1)
	str := string(result.(lua.LString))
	expected := "registry.History{}"
	if str != expected {
		t.Errorf("expected %s, got %s", expected, str)
	}
}

func TestFindHistoryVersionUsesDirectLookup(t *testing.T) {
	storage := historymem.New()
	v1 := version.FromParent(version.New(regapi.RootVersion), 1)
	if err := storage.Save(v1, nil, false); err != nil {
		t.Fatal(err)
	}

	got, err := findHistoryVersion(&lookupOnlyHistory{Storage: storage}, v1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID() != v1.ID() {
		t.Fatalf("direct lookup returned %#v", got)
	}
}

type contextualHistory struct{ *historymem.Storage }

func (h *contextualHistory) VersionsContext(ctx context.Context) ([]regapi.Version, error) {
	return nil, ctx.Err()
}
func (h *contextualHistory) GetVersionContext(ctx context.Context, _ uint) (regapi.Version, error) {
	return nil, ctx.Err()
}
func (h *contextualHistory) GetContext(ctx context.Context, _ regapi.Version) (regapi.ChangeSet, error) {
	return nil, ctx.Err()
}

func TestFindHistoryVersionPassesCallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := findHistoryVersionContext(ctx, &contextualHistory{Storage: historymem.New()}, 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled lookup, got %v", err)
	}
}
