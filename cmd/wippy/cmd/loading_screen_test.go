// SPDX-License-Identifier: MPL-2.0

package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/wippyai/runtime/api/boot"
)

func TestLoadingScreenConfigurationIsOptInAndQuietOnly(t *testing.T) {
	cfg := boot.NewConfig(boot.WithSection("terminal", map[string]any{"splash": "assets/startup.png"}))
	path, err := loadingImagePath(cfg, true)
	if err != nil || path != "assets/startup.png" {
		t.Fatalf("config lost: %q, %v", path, err)
	}
	path, err = loadingImagePath(cfg, false)
	if err != nil || path != "" {
		t.Fatal("must not cover verbose console logs")
	}
	cfg = boot.NewConfig(boot.WithSection("terminal", map[string]any{"splash": true}))
	if _, err := loadingImagePath(cfg, true); err == nil {
		t.Fatal("bad config silently ignored")
	}
	if path, err := loadingImagePath(nil, true); err != nil || path != "" {
		t.Fatal("absent image changes startup")
	}
}

func TestLoadingScreenReadyDoesNotCancelRunningApplication(t *testing.T) {
	app, cancel := context.WithCancel(context.Background())
	signals, stop := context.WithCancel(context.Background())
	callback := context.AfterFunc(signals, cancel)
	screen := &loadingScreen{cancel: cancel, stopSignals: stop, stopCancel: callback}
	screen.Ready()
	screen.Ready()
	select {
	case <-app.Done():
		t.Fatal("stopping the startup signal watcher canceled the app")
	case <-time.After(10 * time.Millisecond):
	}
	screen.Close()
	if app.Err() == nil {
		t.Fatal("final cleanup must cancel the bootstrap context")
	}
}
