// SPDX-License-Identifier: MPL-2.0

// Package toml exposes structural TOML composition to Lua.
package toml

import (
	"bytes"
	"fmt"
	"reflect"

	tomllib "github.com/pelletier/go-toml/v2"
	lua "github.com/wippyai/go-lua"
	luaapi "github.com/wippyai/runtime/api/runtime/lua"
)

const (
	maxDocumentBytes    = 256 * 1024
	maxSourceBytes      = 256 * 1024
	maxOutputBytes      = 512 * 1024
	maxPathSegments     = 32
	maxPathSegmentBytes = 128
	maxPathBytes        = 1024
	maxStructureDepth   = 64
	maxStructureEntries = 16384
)

// Module is the toml module definition.
var Module = &luaapi.ModuleDef{
	Name:        "toml",
	Description: "TOML document composition",
	Class:       []string{luaapi.ClassEncoding, luaapi.ClassDeterministic},
	Build: func() (*lua.LTable, []luaapi.YieldType) {
		mod := lua.CreateTable(0, 1)
		mod.RawSetString("insert", lua.LGoFunc(insertFunc))
		mod.Immutable = true
		return mod, nil
	},
	Types: ModuleTypes,
}

// insert adds the exact subtree selected from source to a document. Existing
// target data is never replaced. Decoding and encoding stay in Go so TOML date
// and time values retain their native types.
func insertFunc(l *lua.LState) int {
	document, ok := l.Get(1).(lua.LString)
	if !ok {
		return invalidError(l, "document string expected")
	}
	if len(document) > maxDocumentBytes {
		return invalidError(l, "document exceeds byte limit")
	}
	path, pathErr := stringPath(l.Get(2))
	if pathErr != nil {
		return invalidError(l, pathErr.Error())
	}
	source, ok := l.Get(3).(lua.LString)
	if !ok || source == "" {
		return invalidError(l, "nonempty source string expected")
	}
	if len(source) > maxSourceBytes {
		return invalidError(l, "source exceeds byte limit")
	}

	base := map[string]any{}
	if document != "" {
		if err := tomllib.Unmarshal([]byte(document), &base); err != nil {
			return invalidError(l, "decode document: "+err.Error())
		}
		if err := boundedStructure(base); err != nil {
			return invalidError(l, "document "+err.Error())
		}
	}
	var overlay map[string]any
	if err := tomllib.Unmarshal([]byte(source), &overlay); err != nil {
		return invalidError(l, "decode source: "+err.Error())
	}
	if err := boundedStructure(overlay); err != nil {
		return invalidError(l, "source "+err.Error())
	}
	selected, err := exactSubtree(overlay, path)
	if err != nil {
		return invalidError(l, err.Error())
	}
	if err := insertMissing(base, path, selected); err != nil {
		return conflictError(l, err.Error())
	}

	var output bytes.Buffer
	if err := tomllib.NewEncoder(&output).Encode(base); err != nil {
		return invalidError(l, "encode document: "+err.Error())
	}
	if output.Len() > maxOutputBytes {
		return invalidError(l, "result exceeds byte limit")
	}
	l.Push(lua.LString(output.String()))
	l.Push(lua.LNil)
	return 2
}

func stringPath(value lua.LValue) ([]string, error) {
	table, ok := value.(*lua.LTable)
	if !ok || table.MaxN() == 0 || table.MaxN() > maxPathSegments {
		return nil, fmt.Errorf("path must be a nonempty bounded string list")
	}
	path := make([]string, table.MaxN())
	totalBytes := 0
	for index := 1; index <= table.MaxN(); index++ {
		segment, ok := table.RawGetInt(index).(lua.LString)
		if !ok || segment == "" || len(segment) > maxPathSegmentBytes {
			return nil, fmt.Errorf("path segment %d must be nonempty text", index)
		}
		totalBytes += len(segment)
		if totalBytes > maxPathBytes {
			return nil, fmt.Errorf("path exceeds byte limit")
		}
		path[index-1] = string(segment)
	}
	count := 0
	table.ForEach(func(_, _ lua.LValue) { count++ })
	if count != len(path) {
		return nil, fmt.Errorf("path must be a list without extra fields")
	}
	return path, nil
}

// boundedStructure limits the already byte-bounded decoded representation
// before the recursive TOML encoder sees it. TOML values cannot contain cycles,
// so an iterative walk is sufficient and does not consume the Go stack.
func boundedStructure(root any) error {
	type item struct {
		value reflect.Value
		depth int
	}
	stack := []item{{value: reflect.ValueOf(root)}}
	entries := 0
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		value := current.value
		for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
			if value.IsNil() {
				break
			}
			value = value.Elem()
		}
		if !value.IsValid() {
			continue
		}
		switch value.Kind() {
		case reflect.Map:
			if value.Len() > 0 && current.depth >= maxStructureDepth {
				return fmt.Errorf("exceeds nesting limit")
			}
			entries += value.Len()
			if entries > maxStructureEntries {
				return fmt.Errorf("exceeds entry limit")
			}
			iter := value.MapRange()
			for iter.Next() {
				stack = append(stack, item{value: iter.Value(), depth: current.depth + 1})
			}
		case reflect.Array, reflect.Slice:
			if value.Len() > 0 && current.depth >= maxStructureDepth {
				return fmt.Errorf("exceeds nesting limit")
			}
			entries += value.Len()
			if entries > maxStructureEntries {
				return fmt.Errorf("exceeds entry limit")
			}
			for index := 0; index < value.Len(); index++ {
				stack = append(stack, item{value: value.Index(index), depth: current.depth + 1})
			}
		}
	}
	return nil
}

func exactSubtree(document map[string]any, path []string) (any, error) {
	current := document
	for index, segment := range path {
		if len(current) != 1 {
			return nil, fmt.Errorf("source must contain only %s", dotted(path))
		}
		value, exists := current[segment]
		if !exists {
			return nil, fmt.Errorf("source does not contain %s", dotted(path))
		}
		if index == len(path)-1 {
			return value, nil
		}
		next, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("source path %s is not a table", dotted(path[:index+1]))
		}
		current = next
	}
	panic("nonempty path required")
}

func insertMissing(document map[string]any, path []string, selected any) error {
	current := document
	for index, segment := range path {
		value, exists := current[segment]
		if index == len(path)-1 {
			if exists {
				return fmt.Errorf("TOML path %s already exists", dotted(path))
			}
			current[segment] = selected
			return nil
		}
		if !exists {
			next := map[string]any{}
			current[segment] = next
			current = next
			continue
		}
		next, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("TOML path %s is not a table", dotted(path[:index+1]))
		}
		current = next
	}
	panic("nonempty path required")
}

func dotted(path []string) string {
	var output bytes.Buffer
	for index, segment := range path {
		if index > 0 {
			output.WriteByte('.')
		}
		output.WriteString(segment)
	}
	return output.String()
}

func invalidError(l *lua.LState, message string) int {
	err := lua.NewLuaError(l, message).WithKind(lua.Invalid).WithRetryable(false)
	l.Push(lua.LNil)
	l.Push(err)
	return 2
}

func conflictError(l *lua.LState, message string) int {
	err := lua.NewLuaError(l, message).WithKind(lua.Conflict).WithRetryable(false)
	l.Push(lua.LNil)
	l.Push(err)
	return 2
}
