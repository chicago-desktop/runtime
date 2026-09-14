<!-- SPDX-License-Identifier: MPL-2.0 -->

# TOML

The deterministic `toml` module provides one structural composition operation.
It does not expose a general Lua table decoder or encoder.

## Loading

```lua
local toml = require("toml")
```

## `toml.insert(document, path, source)`

Parses `document` and `source`, selects the subtree at `path` from `source`, and
inserts it into `document`. `source` must contain only that subtree. The target
must be absent; existing data is never replaced.

```lua
local result, err = toml.insert(existing, {"mcp_servers", "bee"}, [[
[mcp_servers.bee]
url = "http://127.0.0.1:4321/mcp/action"
]])
```

On success it returns the composed TOML string and `nil`. Parsing and encoding
remain in Go, preserving TOML date and time value types. Encoding normalizes
formatting and key order and does not preserve comments.

Inputs are bounded to 256 KiB each. The path has at most 32 segments, each at
most 128 bytes and 1024 bytes in total. Decoded values have at most 16,384
aggregate map/array entries and 64 levels. The encoded result is at most 512 KiB.

Invalid arguments, malformed TOML, a missing or broader source subtree, and
limit failures return `errors.INVALID` with `retryable() == false`. An existing
target or scalar intermediate target returns `errors.CONFLICT` with
`retryable() == false`.
