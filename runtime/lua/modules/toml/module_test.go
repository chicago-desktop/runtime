// SPDX-License-Identifier: MPL-2.0

package toml

import (
	"bytes"
	"fmt"
	"math"
	"testing"
	"time"

	tomllib "github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/require"
	lua "github.com/wippyai/go-lua"
	"github.com/wippyai/runtime/runtime/lua/engine"
)

func newState(t *testing.T) *lua.LState {
	t.Helper()
	state := lua.NewState()
	t.Cleanup(state.Close)
	lua.OpenErrors(state)
	engine.LoadModuleDef(state, Module)
	return state
}

func TestInsertPreservesConfigurationAndNativeValues(t *testing.T) {
	state := newState(t)
	require.NoError(t, state.DoString(`
		local source = [=[
[ui]
screen_mode = "minimal"
released = 1979-05-27T07:32:00Z

[mcp_servers.existing]
url = "https://example.test/mcp"

[[hooks.Stop]]
matcher = ""
]=]
		local bee = [=[
[mcp_servers.bee]
url = "http://127.0.0.1:32123/mcp/action"
enabled = true

[mcp_servers.bee.headers]
Authorization = "Bearer ${BEE_TOKEN}"
]=]
		local encoded, encode_error = toml.insert(source, {"mcp_servers", "bee"}, bee)
		assert(encoded and encode_error == nil, tostring(encode_error))
		result = encoded
	`))

	var decoded map[string]any
	err := tomllib.Unmarshal([]byte(state.GetGlobal("result").String()), &decoded)
	require.NoError(t, err)
	require.Equal(t, "minimal", decoded["ui"].(map[string]any)["screen_mode"])
	require.Equal(t, time.Date(1979, 5, 27, 7, 32, 0, 0, time.UTC), decoded["ui"].(map[string]any)["released"])
	servers := decoded["mcp_servers"].(map[string]any)
	require.Equal(t, "https://example.test/mcp", servers["existing"].(map[string]any)["url"])
	require.Equal(t, "Bearer ${BEE_TOKEN}", servers["bee"].(map[string]any)["headers"].(map[string]any)["Authorization"])
	require.Equal(t, "", decoded["hooks"].(map[string]any)["Stop"].([]any)[0].(map[string]any)["matcher"])
}

func TestInsertPreservesTOMLValueKinds(t *testing.T) {
	state := newState(t)
	require.NoError(t, state.DoString(`
		local source = [=[
[types]
offset = 1979-05-27T07:32:00Z
local_datetime = 1979-05-27T07:32:00
local_date = 1979-05-27
local_time = 07:32:00
integer = 42
float = 1.25
positive_infinity = inf
not_a_number = nan
boolean = true
escaped = "line\nvalue"
inline = { enabled = true, count = 2 }
array = [1, 2, 3]
dates = [1979-05-27, 1980-05-27]

[[types.items]]
name = "first"

["quoted.key"."space key"]
value = "unicode-λ"
]=]
		local bee = [=[
["mcp.servers"."bee server"]
url = "http://127.0.0.1/mcp"
]=]
		local encoded, encode_error = toml.insert(source, {"mcp.servers", "bee server"}, bee)
		assert(encoded and encode_error == nil, tostring(encode_error))
		result = encoded
	`))

	var before, after map[string]any
	err := tomllib.Unmarshal([]byte(`
[types]
offset = 1979-05-27T07:32:00Z
local_datetime = 1979-05-27T07:32:00
local_date = 1979-05-27
local_time = 07:32:00
integer = 42
float = 1.25
positive_infinity = inf
not_a_number = nan
boolean = true
escaped = "line\nvalue"
inline = { enabled = true, count = 2 }
array = [1, 2, 3]
dates = [1979-05-27, 1980-05-27]
[[types.items]]
name = "first"
["quoted.key"."space key"]
value = "unicode-λ"
`), &before)
	require.NoError(t, err)
	err = tomllib.Unmarshal([]byte(state.GetGlobal("result").String()), &after)
	require.NoError(t, err)
	beforeTypes := before["types"].(map[string]any)
	afterTypes := after["types"].(map[string]any)
	require.True(t, math.IsNaN(beforeTypes["not_a_number"].(float64)))
	require.True(t, math.IsNaN(afterTypes["not_a_number"].(float64)))
	delete(beforeTypes, "not_a_number")
	delete(afterTypes, "not_a_number")
	require.Equal(t, beforeTypes, afterTypes)
	require.Equal(t, before["quoted.key"], after["quoted.key"])
	require.Equal(t, "http://127.0.0.1/mcp", after["mcp.servers"].(map[string]any)["bee server"].(map[string]any)["url"])
}

func TestInsertRefusesExistingTarget(t *testing.T) {
	state := newState(t)
	require.NoError(t, state.DoString(`
		local result, err = toml.insert(
			"[mcp_servers.bee]\nurl = \"https://user.example/mcp\"\n",
			{"mcp_servers", "bee"},
			"[mcp_servers.bee]\nurl = \"https://bee.example/mcp\"\n")
		assert(result == nil and err:kind() == errors.CONFLICT and err:retryable() == false)
		local scalar, scalar_error = toml.insert(
			"mcp_servers = \"occupied\"\n",
			{"mcp_servers", "bee"},
			"[mcp_servers.bee]\nurl = \"https://bee.example/mcp\"\n")
		assert(scalar == nil and scalar_error:kind() == errors.CONFLICT and scalar_error:retryable() == false)
	`))
}

func TestInsertRejectsInvalidOrBroadSource(t *testing.T) {
	state := newState(t)
	require.NoError(t, state.DoString(`
		local invalid, invalid_error = toml.insert("[broken", {"mcp_servers", "bee"}, "[mcp_servers.bee]\nurl=\"x\"\n")
		assert(invalid == nil and invalid_error:kind() == errors.INVALID)
		local broad, broad_error = toml.insert("", {"mcp_servers", "bee"}, "[mcp_servers.bee]\nurl=\"x\"\n[ui]\ntheme=\"dark\"\n")
		assert(broad == nil and broad_error:kind() == errors.INVALID)
		local path, path_error = toml.insert("", {"mcp_servers", bee = true}, "[mcp_servers.bee]\nurl=\"x\"\n")
		assert(path == nil and path_error:kind() == errors.INVALID)
		local missing, missing_error = toml.insert("", {"mcp_servers", "bee"}, "[mcp_servers.other]\nurl=\"x\"\n")
		assert(missing == nil and missing_error:kind() == errors.INVALID)
		local scalar, scalar_error = toml.insert("", {"mcp_servers", "bee"}, "mcp_servers=\"x\"\n")
		assert(scalar == nil and scalar_error:kind() == errors.INVALID)
		local sibling, sibling_error = toml.insert("", {"mcp_servers", "bee"}, "[mcp_servers.bee]\nurl=\"x\"\n[mcp_servers.other]\nurl=\"y\"\n")
		assert(sibling == nil and sibling_error:kind() == errors.INVALID)
		local extra, extra_error = toml.insert("", {"mcp_servers", "bee"}, "[mcp_servers.bee]\nurl=\"x\"\n[ui]\ntheme=\"dark\"\n")
		assert(extra == nil and extra_error:kind() == errors.INVALID)
	`))
}

func TestInsertInputAndPathLimits(t *testing.T) {
	state := newState(t)
	require.NoError(t, state.DoString(`
		local source = "[mcp_servers.bee]\nurl=\"x\"\n"
		for _, arguments in ipairs({
			{false, {"mcp_servers", "bee"}, source},
			{"", false, source},
			{"", {"mcp_servers", "bee"}, false},
			{"", {}, source},
			{"", {[2] = "bee"}, source},
			{"", {"mcp_servers", bee = true}, source},
			{"", {string.rep("x", 129)}, "[\"" .. string.rep("x", 129) .. "\"]\nvalue=1\n"},
		}) do
			local result, err = toml.insert(arguments[1], arguments[2], arguments[3])
			assert(result == nil and err:kind() == errors.INVALID and err:retryable() == false)
		end
		local path = {}
		for index = 1, 32 do path[index] = "p" .. tostring(index) end
		local header = "[" .. table.concat(path, ".") .. "]\nvalue=1\n"
		local result, err = toml.insert("", path, header)
		assert(result and err == nil)
		path[33] = "overflow"
		local overflow, overflow_error = toml.insert("", path, header)
		assert(overflow == nil and overflow_error:kind() == errors.INVALID)
		local long_path = {}
		for index = 1, 8 do long_path[index] = string.rep(string.char(96 + index), 128) end
		local long_header = "[" .. table.concat(long_path, ".") .. "]\nvalue=1\n"
		local long_result, long_error = toml.insert("", long_path, long_header)
		assert(long_result and long_error == nil)
		long_path[9] = "z"
		local too_long, too_long_error = toml.insert("", long_path, long_header)
		assert(too_long == nil and too_long_error:kind() == errors.INVALID)
	`))

	oversizedDocument := `result, failure = toml.insert(string.rep("x", 262145), {"a"}, "[a]\nx=1\n")`
	require.NoError(t, state.DoString(oversizedDocument))
	require.Equal(t, lua.LNil, state.GetGlobal("result"))
	oversizedSource := `result, failure = toml.insert("", {"a"}, string.rep("x", 262145))`
	require.NoError(t, state.DoString(oversizedSource))
	require.Equal(t, lua.LNil, state.GetGlobal("result"))
}

func TestInsertRejectsDeepOrWideValues(t *testing.T) {
	state := newState(t)
	deep := ""
	for index := 0; index < maxStructureDepth+1; index++ {
		deep += "["
	}
	deep += "1"
	for index := 0; index < maxStructureDepth+1; index++ {
		deep += "]"
	}
	require.NoError(t, state.DoString(`
		result, failure = toml.insert("", {"a"}, "[a]\nvalue=" .. `+fmt.Sprintf("%q", deep)+` .. "\n")
		assert(result == nil and failure:kind() == errors.INVALID)
	`))

	var wide bytes.Buffer
	wide.WriteString("[a]\nvalues=[")
	for index := 0; index < maxStructureEntries+1; index++ {
		if index > 0 {
			wide.WriteByte(',')
		}
		wide.WriteByte('1')
	}
	wide.WriteString("]\n")
	state.SetGlobal("wide", lua.LString(wide.String()))
	require.NoError(t, state.DoString(`
		result, failure = toml.insert("", {"a"}, wide)
		assert(result == nil and failure:kind() == errors.INVALID)
	`))
}
