// SPDX-License-Identifier: MPL-2.0

package lua

import (
	"context"

	"github.com/wippyai/runtime/api/boot"
	tomlmod "github.com/wippyai/runtime/runtime/lua/modules/toml"
)

// TOML registers the deterministic TOML codec.
func TOML() boot.Component {
	return boot.New(boot.P{
		Name:      TOMLName,
		DependsOn: []boot.Name{EngineName},
		Load: func(ctx context.Context) (context.Context, error) {
			manager := GetCodeManager(ctx)
			if manager == nil {
				return ctx, nil
			}
			if err := AddModules(ctx, manager, tomlmod.Module); err != nil {
				return ctx, err
			}
			return ctx, nil
		},
	})
}
