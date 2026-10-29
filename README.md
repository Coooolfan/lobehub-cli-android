# lobe-device

English | [简体中文](README.zh-CN.md)

A personal LobeHub device client for rooted arm64 Android. Connect with an API key to provide
file operations, shell commands, and system metrics through the device gateway.
Deploy a single binary; commands run with `/system/bin/sh`.

## Build

Requires Go 1.24 or later. Dependencies are pinned in `go.mod` and `go.sum`.

```sh
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -trimpath -buildvcs=false -o lobe-device .
```

`-trimpath` removes local source paths, and `-buildvcs=false` disables embedded Git build information.
The binary uses Android PIE format with `/system/bin/linker64` as its interpreter and has no
`DT_NEEDED` shared-library dependencies. No Android NDK, cgo, or glibc is required.

## Run

Install through adb and start as root:

```sh
adb push lobe-device /data/local/tmp/lobe-device
adb shell su -c 'chmod 700 /data/local/tmp/lobe-device'
adb shell
su
/data/local/tmp/lobe-device \
  --server https://app.lobehub.com \
  --api-key '<full-access-api-key>' \
  --state-dir /data/local/tmp/lobe-device-state
```

| Flag | Description |
| --- | --- |
| `--server` | Required. LobeHub server base URL, including any path prefix. |
| `--api-key` | Required. Personal API key. Device registration requires scope `*` or no configured scope. |
| `--gateway` | Optional. HTTP(S) gateway base URL, such as `https://example.com/edge/gw`. |
| `--state-dir` | Identity directory. Defaults to `$HOME/.lobe-device`. |

Without `--gateway`, the client anonymously requests `/trpc/lambda/config.getGlobalConfig` and reads
`result.data.json.serverConfig.deviceGatewayUrl`. It exits if discovery fails or the address is
missing or invalid. The WebSocket endpoint appends `/ws` to the gateway base URL, preserving its path prefix.

### HOME and working directory

The client requires a writable HOME. If the HOME environment variable is missing or its directory
is not writable, it tries `/root`, then `/data/local/tmp/lobe-device-home`, creating the directory
and setting HOME. It creates Desktop, Documents, and Downloads directories under HOME.
HOME is the default working directory and is inherited by shell commands.

### Device identity

`identity.json` stores deviceId and connectionId with `0600` permissions, file locking, and atomic writes.

- **deviceId:** Derived from `ro.serialno`, or `ro.boot.serialno` when unavailable, combined with userId
  and the LobeHub salt using SHA256. The first 32 hexadecimal characters identify the device, registered
  as `machine-id`. The same device and user produce the same deviceId. Raw hardware serials are neither
  uploaded nor logged.
- **connectionId:** A UUID generated once and persisted. Processes sharing a state directory reuse it.
- **Fallback:** If hardware identification is unavailable, the client retains its stored identity.
  Without an existing identity, it generates a UUID and registers as `fallback`; the device page shows
  an unstable identity notice. A corrupt identity file causes the client to exit.

A second process using the same state directory replaces the existing connection.
A different state directory generates a new connectionId. Server-side device records are managed by the user.

### Authentication and connections

Login uses a Bearer key, device registration uses `X-API-Key`, and the first WebSocket frame carries
API key authentication. The client does not persist the key or put it in logs or the WebSocket URL.
Registration failure does not prevent a gateway connection.

Heartbeats start after authentication and run every 30 seconds. Three consecutive unacknowledged
heartbeats trigger reconnection. Other disconnections retry with a backoff of 1 / 2 / 4 / 8 / 16 / 30 seconds.
`auth_expired` does not trigger token refresh or disconnection.

- `auth_failed`: Exit with a nonzero status.
- `Replaced by new connection`: Exit normally.
- Ctrl-C / SIGTERM: Exit and terminate managed command process groups.

Boot startup and process supervision are managed externally.

## Android DNS

The client checks nameservers in `/etc/resolv.conf`, then `getprop` properties `net.*dns1` through
`net.*dns4`, and finally public resolvers `1.1.1.1` and `8.8.8.8`.

Override DNS with `LOBE_DEVICE_DNS`, using comma-separated IPv4 / IPv6 addresses with optional ports:

```sh
export LOBE_DEVICE_DNS='192.168.1.1,223.5.5.5'
```

This setting applies only to the client's Go network requests.

## System metrics

The client reads `/proc/stat`, `/proc/meminfo`, and `/proc/loadavg`, sends `device_metrics` frames
in `DeviceMetricSample` format, and waits for `device_metrics_ack`.

- **CPU usage:** Calculated from changes in aggregate CPU counters, excluding idle and iowait and
  avoiding double-counting guest time.
- **Memory usage:** `MemTotal - MemAvailable`.
- **Load averages:** 1 / 5 / 15 minute averages, along with the CPU core count.
- **Sampling:** Establish a baseline at startup, calculate the first CPU reading after one second,
  then sample every 60 seconds.
- **Missing values:** Unknown CPU or load values are `null`. A failed CPU or memory read skips the
  sample and logs the cause.

Sampling continues while disconnected with `connected=false`. Pending samples remain in memory
for up to two days and upload after reconnection in batches of up to 500. Acknowledged samples are
removed; batches without an acknowledgement retry after 10 seconds. Pending samples do not survive a process restart.

## Tool interface

Requests use `identifier: lobe-local-system`; `arguments` is a JSON object encoded as a string.
Relative paths resolve against `cwd`. An omitted `cwd` defaults to HOME; a relative `cwd` resolves against HOME.

| Tool | Arguments | Alias |
| --- | --- | --- |
| `runCommand` | `command`, `run_in_background` | — |
| `getCommandOutput` | `shell_id` | — |
| `killCommand` | `shell_id` | — |
| `readFile` | `path`, optional `loc` | `readLocalFile` |
| `writeFile` | `path`, `content` | `writeLocalFile` |
| `editFile` | `file_path`, `old_string`, `new_string`, `replace_all` | `editLocalFile` |
| `listFiles` | `path` | `listLocalFiles` |
| `grepContent` | `pattern`, `path` or `cwd` | — |
| `globFiles` | `pattern`, `scope` | `globLocalFiles` |

### File operations

- `readFile.loc` is a zero-based, end-exclusive line range.
- `writeFile` creates parent directories as needed.
- `editFile` fails when no match is found; multiple matches require `replace_all=true`.
- `globFiles` supports `**`. The search root uses `scope`, with `path` and `directory` also accepted.
- `grepContent` uses Go RE2 regular expressions and supports `-i`. The search root is chosen from
  `path`, `scope`, `directory`, then `cwd`. Results include full paths and one-based line numbers.
  Searches do not follow directory symlinks and report the count of skipped unreadable, binary, or oversized files.
- Reads and writes support regular files only. Reading a file is limited to 4 MiB; use shell commands for larger files.

### Command execution

`run_in_background=true` returns a stable `shell_id` immediately after startup; `background` is also accepted.
Polling and termination use `shell_id`, with `commandId` also accepted. Output accumulates in the client
process, and polling does not clear it. Each command retains up to 4 MiB of stdout and stderr separately,
with explicit truncation markers. Completed commands remain available for polling.

Commands accept an optional `env`. HOME always uses the writable directory selected by the client.
The frame's `timeout` is in milliseconds and defaults to 120 seconds; `args.timeout` can shorten the
execution deadline. A timeout kills the command process group and returns a failure. Background commands
are subject to the same execution deadline. The execution budget reserves up to 100ms for response
transmission, or 10% for small budgets.

Background commands survive gateway reconnections. Their shell_id values expire when the client process exits.
The client executes up to 32 concurrent tool requests and immediately returns busy when that limit is exceeded.

### Result format

Tool responses include `executionTimeMs`. Command `content` contains shell_id, exit code, running status,
and output; card `state` uses `RunCommandState` or `GetCommandOutputState` fields.
File `content` includes the path, text, or error cause. Unsupported tools return empty content,
`success=false`, and an explicit error.

Heterogeneous Agent, RPC, message API, and HTTP/WebSocket tunnel requests receive rejection responses.
Browser tools, Computer Use, PTY, sandboxing, workspaces, JWT / token refresh, and Magisk are unsupported.
File and shell tools run with the Android permissions of the user starting the client.

## Development checks

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -trimpath -buildvcs=false -o lobe-device .
go run ./scripts/check-elf.go lobe-device
```

Host tests use `/bin/sh` for file and subprocess operations and local HTTP / WebSocket servers for
protocol checks. The Android client always uses `/system/bin/sh`.
