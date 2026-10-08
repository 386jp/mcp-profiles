# mcp-profiles

An MCP proxy that bundles several MCP servers exposing the same tools, and routes each tool call to one of them by a `profile` argument.

Define environments (dev / stg / prod), Slack workspaces, or different credentials as profiles, and keep using your existing MCP servers as they are.

```
Claude Code ──(stdio | streamable HTTP)──> mcp-profiles ─┬──> upstream "dev"
                                                         ├──> upstream "stg"
                                                         └──> upstream "prod"
```

## Usage

Write a [configuration file](#configuration), then register mcp-profiles as an MCP server.

### Install

Download the archive for your platform (macOS or Linux, amd64 or arm64) from [Releases](https://github.com/386jp/mcp-profiles/releases), and put `mcp-profiles` on your `PATH`. `checksums.txt` holds the SHA-256 of each archive.

```sh
gh release download --repo 386jp/mcp-profiles --pattern mcp-profiles_darwin_arm64.tar.gz
tar -xzf mcp-profiles_darwin_arm64.tar.gz
```

Archive names do not include the version, so the latest release is always at `releases/latest/download/mcp-profiles_<os>_<arch>.tar.gz`.

The macOS binaries are not notarized. If macOS blocks a binary downloaded with a browser, remove the quarantine attribute with `xattr -d com.apple.quarantine mcp-profiles`.

### Claude Code

Started over stdio:

```json
{
  "mcpServers": {
    "example": {
      "type": "stdio",
      "command": "/path/to/mcp-profiles",
      "env": { "MCP_PROFILES_CONFIG": "/path/to/mcp-profiles.json" }
    }
  }
}
```

Listening over HTTP:

```json
{
  "mcpServers": {
    "example": {
      "type": "http",
      "url": "http://127.0.0.1:8000/mcp",
      "headers": { "Authorization": "Bearer ${MCP_PROFILES_TOKEN}" }
    }
  }
}
```

### Docker

Images are published to GitHub Container Registry:

```sh
docker pull ghcr.io/386jp/mcp-profiles:latest
```

```sh
docker run --rm -p 8000:8000 \
  -e MCP_PROFILES_TOKEN=... \
  -v ./mcp-profiles.json:/etc/mcp-profiles/config.json:ro \
  ghcr.io/386jp/mcp-profiles:latest
```

Inside a container, set `listen.addr` to `0.0.0.0:8000` and protect it with `listen.headers`.

The distroless image has no shell and no runtimes such as Node.js, so stdio upstreams (for example ones started with `npx`) do not work in it. If you need them, copy the binary into an image that has the runtime, and run it with `docker run --init`.

## Configuration

mcp-profiles is configured entirely by a JSON file. Its path is read from the `MCP_PROFILES_CONFIG` environment variable, or `mcp-profiles.json` in the current directory if it is not set.

```json
{
  "defaultProfile": "dev",
  "listProfilesTool": "list_profiles",
  "reconnectTool": "reconnect_profile",
  "profiles": {
    "dev": {
      "description": "Development. Writes are fine.",
      "type": "http",
      "url": "https://dev.example.com/mcp",
      "headers": { "Authorization": "Bearer ${DEV_TOKEN}" }
    },
    "prod": {
      "description": "Production. Be careful with writes.",
      "type": "stdio",
      "command": "example-mcp-server",
      "args": ["--env", "prod"],
      "env": { "API_TOKEN": "${PROD_TOKEN}" }
    }
  }
}
```

### Top level

| Key | Default | Description |
|---|---|---|
| `listen` | `{"type": "stdio"}` | How clients connect (see below) |
| `profiles` | (required) | Map from profile name to upstream definition |
| `profileArg` | `"profile"` | Name of the argument added to every tool. Startup fails if it collides with an upstream argument |
| `defaultProfile` | none | Profile used when `profile` is omitted. Without it, `profile` is required |
| `listProfilesTool` | none | Name of the profile listing tool. Not exposed if omitted |
| `reconnectTool` | none | Name of the reconnect tool. Not exposed if omitted |
| `log` | `{"level": "info", "format": "text"}` | `level` is `debug` / `info` / `warn` / `error`; `format` is `text` / `json`. Logs always go to stderr |
| `timeouts` | see below | Go duration strings (`30s`, `2m`). `0s` means no timeout |
| `lazy` | `false` | Connect upstreams on first use (see [Lazy mode](#lazy-mode)) |

| `timeouts` key | Default | Applies to |
|---|---|---|
| `startup` | `30s` | Connecting to one upstream and listing its tools. Also used by the reconnect tool |
| `call` | `0s` | One `tools/call` to an upstream |
| `shutdown` | `10s` | Waiting for in-flight calls after SIGTERM / SIGINT |

### profiles

Each entry has the same shape as an `.mcp.json` server entry.

| Key | Type | Description |
|---|---|---|
| `type` | all | `stdio` or `http` (streamable HTTP) |
| `description` | all | Description returned by the profile listing tool. Setting it without `listProfilesTool` is an error |
| `url` / `headers` | `http` | Endpoint, and headers added to every request |
| `command` / `args` / `env` | `stdio` | Command to start. `env` is added to the proxy's environment |

`${VAR}` and `${VAR:-default}` in `url`, `headers`, `command`, `args` and `env` are expanded from environment variables. An unset variable without a default is an error.

### listen

```json
{ "type": "stdio" }
{ "type": "http", "addr": "127.0.0.1:8000", "path": "/mcp", "headers": { "Authorization": "Bearer ${MCP_PROFILES_TOKEN}" } }
```

- For `type: "http"`, `addr` defaults to `127.0.0.1:8000` and `path` to `/mcp`.
- With `headers`, only requests carrying all of them with matching values are accepted; others get `401`.
- Binding to a non-loopback address without `headers` logs a warning at startup.

### Lazy mode

By default, every upstream is connected at startup and stays connected. With many profiles, especially stdio upstreams that each start a process, set `lazy` to connect them only when needed:

```json
"lazy": true
"lazy": { "maxConnected": 2, "keepBase": true, "idleTimeout": "10m" }
```

- At startup, only the base profile (`defaultProfile`, or the first profile name in sorted order) is connected, to read the tools. It is disconnected right after, unless `keepBase` is `true`.
- A profile is connected on its first call, and its tools are checked against the base profile's then. A mismatch disables the profile instead of failing the startup.
- Only the `maxConnected` most recently used profiles stay connected (default `1`). When another profile is called, the least recently used connected one is disconnected once its running calls finish.
- With `idleTimeout`, the connected profile is also disconnected after that long without calls. Defaults to `0s` (never).
- Disconnected profiles show up as `idle`. Profiles disabled by a failure still need the reconnect tool.

`"lazy": true` is the same as `"lazy": {}`: `maxConnected` defaults to `1` and `keepBase` to `false`.

## Builtin tools

| Tool | Description |
|---|---|
| Profile listing (`listProfilesTool`) | Returns each profile's name, `description`, status (`active` / `idle` / `reconnecting` / `disabled`) and the reason it was disabled |
| Reconnect (`reconnectTool`) | Reconnects a profile, and makes it `active` again if its tools still match the ones seen at startup |

## Upstream failures and changes

- When the process of a stdio upstream exits, its profile becomes `disabled`.
- When an upstream notifies that its tool list changed and the tool names or an `inputSchema` differ from startup, its profile becomes `disabled` too.
- Calls to a `disabled` profile return an error. Profiles never recover on their own; use the reconnect tool or restart the proxy.
- A failed request to a streamable HTTP upstream does not disable its profile.

## How it works

- At startup, mcp-profiles connects to the upstream of every profile and checks that they expose the same tool names and the same `inputSchema` for each tool. Any mismatch fails the startup. In [lazy mode](#lazy-mode), this check happens when each profile is first used.
- Every tool is exposed with an extra `profile` argument whose `enum` lists the profile names.
- On `tools/call`, the `profile` argument is removed and the call is forwarded as is to the selected upstream.
- Each upstream can use its own transport. stdio and streamable HTTP can be mixed.

## Limitations

- Targets MCP specification 2026-07-28. Clients must connect with 2026-07-28. Upstreams on older versions are used on a best-effort basis with a warning, and are not guaranteed to work.
- Only tools are handled. Resources and prompts are not exposed.
- OAuth is not supported. For upstreams that require OAuth, put something like [mcp-remote](https://github.com/geelen/mcp-remote) in between as a stdio upstream.
- The HTTP+SSE transport (`type: "sse"`) is not supported.

## Development

Uses [mise](https://mise.jdx.dev/).

```sh
mise run setup   # download dependencies
mise run build   # build bin/mcp-profiles
mise run test
mise run lint
```
