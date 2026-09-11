// SPDX-License-Identifier: MPL-2.0

package registry

import (
	lua "github.com/wippyai/go-lua"
	"github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/runtime/lua/engine/value"
)

// versionID returns the ID of a version
func versionID(l *lua.LState) int {
	ud := l.CheckUserData(1)
	version, ok := ud.Value.(registry.Version)
	if !ok {
		l.ArgError(1, "version expected")
		return 0
	}

	l.Push(lua.LNumber(version.ID()))
	return 1
}

// versionPrevious returns the previous version
func versionPrevious(l *lua.LState) int {
	ud := l.CheckUserData(1)
	version, ok := ud.Value.(registry.Version)
	if !ok {
		l.ArgError(1, "version expected")
		return 0
	}

	linked, err := historyLinkedVersion(l, version)
	if err != nil {
		l.Push(lua.LNil)
		l.Push(lua.WrapErrorWithLua(l, err, "read version links"))
		return 2
	}
	prev := linked.Previous()
	if prev == nil {
		l.Push(lua.LNil)
		return 1
	}

	value.PushTypedUserData(l, prev, typeVersion)
	return 1
}

// versionString returns a string representation of the version
func versionString(l *lua.LState) int {
	ud := l.CheckUserData(1)
	version, ok := ud.Value.(registry.Version)
	if !ok {
		l.ArgError(1, "version expected")
		return 0
	}

	l.Push(lua.LString(version.String()))
	return 1
}

// versionNext returns the next version
func versionNext(l *lua.LState) int {
	ud := l.CheckUserData(1)
	version, ok := ud.Value.(registry.Version)
	if !ok {
		l.ArgError(1, "version expected")
		return 0
	}

	linked, err := historyLinkedVersion(l, version)
	if err != nil {
		l.Push(lua.LNil)
		l.Push(lua.WrapErrorWithLua(l, err, "read version links"))
		return 2
	}
	next := linked.Next()
	if next == nil {
		l.Push(lua.LNil)
		return 1
	}

	value.PushTypedUserData(l, next, typeVersion)
	return 1
}

func historyLinkedVersion(l *lua.LState, stored registry.Version) (registry.Version, error) {
	reg := registry.GetRegistry(l.Context())
	if reg == nil {
		return stored, nil
	}
	if _, ok := reg.History().(registry.PublishedHistory); !ok {
		return stored, nil
	}
	if stored.Previous() != nil || stored.Next() != nil {
		return stored, nil
	}
	if reader, ok := reg.History().(registry.ContextHistory); ok {
		return reader.GetVersionContext(l.Context(), stored.ID())
	}
	return stored, nil
}
