// SPDX-License-Identifier: MPL-2.0

package tty

import (
	"context"
	"errors"
	"image"
	"testing"

	"github.com/stretchr/testify/require"
	lua "github.com/wippyai/go-lua"
	ctxapi "github.com/wippyai/runtime/api/context"
	ttyapi "github.com/wippyai/runtime/api/tty"
)

type viewportTestService struct {
	created  *viewportTestView
	attached *viewportTestView
	handle   string
}

func (s *viewportTestService) Create(context.Context, int, int) (ttyapi.Viewport, error) {
	return s.created, nil
}

func (s *viewportTestService) Attach(_ context.Context, handle string) (ttyapi.Viewport, error) {
	s.handle = handle
	return s.attached, nil
}

func (*viewportTestService) Binding(string) (ttyapi.Binding, error) { return nil, nil }
func (*viewportTestService) Close() error                           { return nil }

type viewportTestView struct {
	grant    string
	handle   string
	sent     []ttyapi.Event
	closeErr error
	snapshot ttyapi.Snapshot
	closed   bool
}

func (v *viewportTestView) Grant() string               { return v.grant }
func (v *viewportTestView) Handle() string              { return v.handle }
func (v *viewportTestView) Snapshot() ttyapi.Snapshot   { return v.snapshot }
func (*viewportTestView) Updates() <-chan ttyapi.Update { return nil }
func (v *viewportTestView) Send(event ttyapi.Event) error {
	v.sent = append(v.sent, event)
	return nil
}
func (*viewportTestView) Resize(int, int) error { return nil }
func (v *viewportTestView) Close() error        { v.closed = true; return v.closeErr }

func TestLuaViewportAttachHandleAndRevisionPolling(t *testing.T) {
	created := &viewportTestView{
		grant: "producer", handle: "viewer",
		snapshot: ttyapi.Snapshot{Revision: 7, Width: 40, Height: 12, Rows: []string{"ready"}},
	}
	attached := &viewportTestView{
		handle:   "viewer",
		snapshot: ttyapi.Snapshot{Revision: 7, Width: 40, Height: 12, Rows: []string{"ready"}},
	}
	service := &viewportTestService{created: created, attached: attached}
	ctx := ttyapi.WithService(ctxapi.NewRootContext(), service)

	l := lua.NewState()
	defer l.Close()
	bindTTY(l)
	l.SetContext(ctx)
	require.NoError(t, l.DoString(`
		local creator, create_err = tty.viewport({width = 40, height = 12})
		assert(creator and not create_err)
		local grant, grant_err = creator:grant()
		assert(grant == "producer" and not grant_err)
		assert(creator:handle() == "viewer")
		assert(creator:send({type = "visibility", visible = true}))
		assert(creator:send({type = "resize", width = 100, height = 30}))
		local first = creator:snapshot()
		assert(first.revision == 7 and first.rows[1] == "ready")
		assert(creator:snapshot(first.revision) == nil)

		local viewer, attach_err = tty.attach(creator:handle())
		assert(viewer and not attach_err)
		local attached_grant, attached_grant_err = viewer:grant()
		assert(attached_grant == nil and attached_grant_err)
		assert(viewer:handle() == "viewer")
		assert(viewer:close())
	`))
	require.Equal(t, "viewer", service.handle)
	require.Equal(t, []ttyapi.Event{
		{Type: "visibility", Visible: true},
		{Type: "resize", Width: 100, Height: 30},
	}, created.sent)
	require.True(t, attached.closed)
}

func TestLuaViewportRejectsMalformedInput(t *testing.T) {
	created := &viewportTestView{}
	service := &viewportTestService{created: created}
	ctx := ttyapi.WithService(ctxapi.NewRootContext(), service)

	l := lua.NewState()
	defer l.Close()
	bindTTY(l)
	l.SetContext(ctx)
	require.NoError(t, l.DoString(`
		local invalid_dimension, dimension_err = tty.viewport({width = 1.5})
		assert(invalid_dimension == nil and dimension_err)
		local oversized, oversized_err = tty.viewport({width = 65535, height = 65535})
		assert(oversized == nil and oversized_err)

		local viewport = assert(tty.viewport())
		local sent, send_err = viewport:send({type = "resize", width = "80", height = 24})
		assert(sent == nil and send_err)
		sent, send_err = viewport:send({type = "visibility", visible = 1})
		assert(sent == nil and send_err)
		sent, send_err = viewport:send({type = "unknown"})
		assert(sent == nil and send_err)
		local resized, resize_err = viewport:resize(80.5, 24)
		assert(resized == nil and resize_err)
		resized, resize_err = viewport:resize(65535, 65535)
		assert(resized == nil and resize_err)
		assert(not pcall(function() viewport:snapshot(1.5) end))
	`))
	require.Empty(t, created.sent)
}

func TestLuaViewportClosePreservesFailure(t *testing.T) {
	created := &viewportTestView{closeErr: errors.New("close failed")}
	service := &viewportTestService{created: created}
	ctx := ttyapi.WithService(ctxapi.NewRootContext(), service)

	l := lua.NewState()
	defer l.Close()
	bindTTY(l)
	l.SetContext(ctx)
	require.NoError(t, l.DoString(`
		local viewport = assert(tty.viewport())
		for _ = 1, 2 do
			local closed, close_err = viewport:close()
			assert(closed == nil and tostring(close_err):find("close failed", 1, true))
		end
	`))
	require.True(t, created.closed)
}

// A viewport carries the pictures standing on the screen, and a picture must
// arrive with the identity it had: a viewer somewhere else recognises it by
// its serial and version, and sends the pixels only for one it has not seen.
// Minting a fresh identity here would make every picture look new on every
// frame, and the pixels would travel again each time — invisible locally,
// ruinous over a network.
func TestLuaViewportSnapshotCarriesPictures(t *testing.T) {
	picture := image.NewRGBA(image.Rect(0, 0, 4, 2))
	view := &viewportTestView{
		grant: "producer", handle: "viewer",
		snapshot: ttyapi.Snapshot{
			Revision: 3, Width: 20, Height: 5, Rows: []string{"under"},
			Placements: []ttyapi.Placement{{
				Image: picture, ID: "wallpaper", Version: 9, Serial: 41,
				Row: 2, Col: 3, Cols: 4, Rows: 2, Z: 1,
			}},
		},
	}
	service := &viewportTestService{created: view, attached: view}
	ctx := ttyapi.WithService(ctxapi.NewRootContext(), service)

	l := lua.NewState()
	defer l.Close()
	bindTTY(l)
	l.SetContext(ctx)
	require.NoError(t, l.DoString(`
		local view = assert(tty.viewport({width = 20, height = 5}))
		local shot = view:snapshot()
		assert(shot.images ~= nil, "a snapshot carries the pictures")
		assert(#shot.images == 1, "one picture")
		local one = shot.images[1]
		assert(one.id == "wallpaper", "named")
		assert(one.x == 3 and one.y == 2, "placed where the producer put it")
		assert(one.cols == 4 and one.rows == 2, "as many cells as it covers")
		assert(one.version == 9 and one.serial == 41, "with the identity it arrived with")
		local w, h = one.raster:size()
		assert(w == 4 and h == 2, "and its pixels")
	`))
}
