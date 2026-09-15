// SPDX-License-Identifier: MPL-2.0

package cmd

import (
	"context"
	"fmt"
	"sync"

	"github.com/wippyai/runtime/api/boot"
	terminalsvc "github.com/wippyai/runtime/service/terminal"
)

type loadingScreen struct {
	splash      *terminalsvc.Splash
	cancel      context.CancelFunc
	stopSignals context.CancelFunc
	stopCancel  func() bool
	ready       sync.Once
}

type loadingScreenKey struct{}

// The image belongs to the deployment, not to a terminal.host entry: those
// entries and their module filesystems do not exist during early bootstrap.
func loadingImagePath(cfg boot.Config, enabled bool) (string, error) {
	if !enabled || cfg == nil {
		return "", nil
	}
	value, present := cfg.Get("terminal.splash")
	if !present {
		return "", nil
	}
	path, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("terminal.splash must be a local image path string")
	}
	return path, nil
}

func startLoadingScreen(ctx context.Context, cfg boot.Config, enabled bool) (context.Context, *loadingScreen, error) {
	path, err := loadingImagePath(cfg, enabled)
	if err != nil || path == "" {
		return ctx, nil, err
	}
	splash, err := terminalsvc.StartSplash(path)
	if err != nil || splash == nil {
		return ctx, nil, err
	}
	bootCtx, cancel := context.WithCancel(ctx)
	screen := &loadingScreen{splash: splash, cancel: cancel}
	signalCtx, stopSignals := newExecSignalContext(ctx)
	screen.stopSignals = stopSignals
	screen.stopCancel = context.AfterFunc(signalCtx, func() {
		_ = splash.Close()
		cancel()
	})
	bootCtx = terminalsvc.WithSplash(bootCtx, splash)
	bootCtx = context.WithValue(bootCtx, loadingScreenKey{}, screen)
	return bootCtx, screen, nil
}

func loadingScreenFrom(ctx context.Context) *loadingScreen {
	screen, _ := ctx.Value(loadingScreenKey{}).(*loadingScreen)
	return screen
}

// Ready hands signal handling to the normal supervisor/exec path, while the
// image remains until the application's first surface frame.
func (s *loadingScreen) Ready() {
	if s == nil {
		return
	}
	s.ready.Do(func() {
		s.stopCancel()
		s.stopSignals()
	})
}

func (s *loadingScreen) Dismiss() {
	if s != nil {
		_ = s.splash.Close()
	}
}

func (s *loadingScreen) Close() {
	if s == nil {
		return
	}
	s.Ready()
	s.Dismiss()
	s.cancel()
}
