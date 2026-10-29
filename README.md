# lobe-device

运行于已 root 的 arm64 Android 的 LobeHub 个人设备客户端。通过 API key 连接设备网关，
提供文件操作、shell 命令和设备指标上报。客户端以单个二进制文件部署，使用 `/system/bin/sh` 执行命令。

## 构建

需要 Go 1.24 或更新版本。依赖版本固定在 `go.mod` 和 `go.sum`。

```sh
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -trimpath -buildvcs=false -o lobe-device .
```

`-trimpath` 去除本机源码路径，`-buildvcs=false` 关闭 Git 构建信息嵌入。
产物采用 Android PIE 格式，解释器为 `/system/bin/linker64`，没有 `DT_NEEDED` 共享库依赖。
构建无需 Android NDK，运行无需 cgo 或 glibc。

## 运行

通过 adb 安装并以 root 身份启动：

```sh
adb push lobe-device /data/local/tmp/lobe-device
adb shell su -c 'chmod 700 /data/local/tmp/lobe-device'
adb shell
su
/data/local/tmp/lobe-device \
  --server https://app.lobehub.com \
  --api-key '<全权限 API key>' \
  --state-dir /data/local/tmp/lobe-device-state
```

| 参数 | 说明 |
| --- | --- |
| `--server` | 必填，LobeHub 服务器根地址，支持路径前缀。 |
| `--api-key` | 必填，个人 API key。设备登记要求 scope 为 `*` 或未设置 scope。 |
| `--gateway` | 可选，HTTP(S) 网关根地址，例如 `https://example.com/edge/gw`。 |
| `--state-dir` | 身份文件目录，默认 `$HOME/.lobe-device`。 |

未指定 `--gateway` 时，客户端匿名请求 `/trpc/lambda/config.getGlobalConfig`，读取
`result.data.json.serverConfig.deviceGatewayUrl`。发现失败、地址缺省或格式非法时退出。
网关根地址保留路径前缀，并以 `/ws` 作为 WebSocket 端点。

### HOME 和工作目录

客户端使用可写的 HOME。环境变量 HOME 缺失或目录不可写时，依次尝试创建 `/root`
和 `/data/local/tmp/lobe-device-home`，并设置 HOME。HOME 下创建 Desktop、Documents 和 Downloads 目录。
默认工作目录为 HOME，shell 命令继承该环境变量。

### 设备身份

`identity.json` 保存 deviceId 和 connectionId，权限为 `0600`，使用文件锁和原子写入。

- deviceId：读取 `ro.serialno`，不可用时读取 `ro.boot.serialno`，将硬件标识、userId
  和 LobeHub 固定 salt 做 SHA256，取前 32 位，登记为 `machine-id`。同一设备和用户得到相同 deviceId。
  原始硬件序列号不上传、不写入日志。
- connectionId：首次生成 UUID 并持久化。同一状态目录下的进程复用该值。
- 硬件标识不可用时，保留已保存的身份；没有可用身份时生成 UUID，登记为 `fallback`，
  设备页面显示身份不稳定提示。身份文件损坏时退出。

同一状态目录启动的第二个进程会替换已有连接。更换状态目录会生成新的 connectionId。
客户端只登记设备，服务端设备记录由用户管理。

### 认证和连接

登录通过 Bearer key 验证身份，设备登记使用 `X-API-Key`，WebSocket 首帧使用 API key 认证。
客户端不持久化 API key，不将其写入日志或 WebSocket URL。登记失败不阻止网关连接。

认证成功后每 30 秒发送心跳，连续 3 个心跳未获确认时重连。普通断线按
1 / 2 / 4 / 8 / 16 / 30 秒退避重试。`auth_expired` 不触发刷新或断线。

- `auth_failed`：退出并返回非零状态。
- `Replaced by new connection`：正常退出。
- Ctrl-C / SIGTERM：退出并终止客户端管理的命令进程组。

开机启动和进程守护由外部管理。

## Android DNS

客户端依次使用 `/etc/resolv.conf` 的 nameserver、`getprop` 的 `net.*dns1` 至 `net.*dns4`
属性，以及公共 DNS `1.1.1.1` 和 `8.8.8.8`。

环境变量 `LOBE_DEVICE_DNS` 可指定 DNS，支持逗号分隔的 IPv4 / IPv6 地址和可选端口：

```sh
export LOBE_DEVICE_DNS='192.168.1.1,223.5.5.5'
```

该配置仅作用于客户端的 Go 网络请求。

## 设备指标

客户端从 `/proc/stat`、`/proc/meminfo` 和 `/proc/loadavg` 读取整机指标，按
`DeviceMetricSample` 格式发送 `device_metrics` 帧，并等待 `device_metrics_ack`。

- CPU 使用率：两次总 CPU 时间的差值，排除 idle 和 iowait，不重复计入 guest 时间。
- 内存使用量：`MemTotal - MemAvailable`。
- 平均负载：1 / 5 / 15 分钟负载，同时上报 CPU 核心数。
- 采样周期：启动时读取基线，1 秒后计算首次 CPU 使用率，之后每 60 秒采样。
- 缺失值：未知 CPU 或负载使用 `null`；CPU 或内存读取失败时跳过该次采样并记录原因。

断线期间继续采样，标记 `connected=false`。待上传样本在进程内最多保存两天，重连后
每批最多补传 500 条。确认后删除对应样本，10 秒未确认时重试。未上传样本不跨进程重启保存。

## 工具接口

工具请求使用 `identifier: lobe-local-system`，`arguments` 为 JSON 对象的字符串。
相对路径按 `cwd` 解析；省略 `cwd` 时使用 HOME；相对 `cwd` 按 HOME 解析。

| 工具 | 参数 | 别名 |
| --- | --- | --- |
| `runCommand` | `command`、`run_in_background` | — |
| `getCommandOutput` | `shell_id` | — |
| `killCommand` | `shell_id` | — |
| `readFile` | `path`、可选 `loc` | `readLocalFile` |
| `writeFile` | `path`、`content` | `writeLocalFile` |
| `editFile` | `file_path`、`old_string`、`new_string`、`replace_all` | `editLocalFile` |
| `listFiles` | `path` | `listLocalFiles` |
| `grepContent` | `pattern`、`path` 或 `cwd` | — |
| `globFiles` | `pattern`、`scope` | `globLocalFiles` |

### 文件操作

- `readFile.loc` 为 0 起始、右端不包含的行范围。
- `writeFile` 自动创建父目录。
- `editFile` 缺失匹配时失败；多处匹配需要 `replace_all=true`。
- `globFiles` 支持 `**`，搜索根目录使用 `scope`，也接受 `path` 或 `directory`。
- `grepContent` 使用 Go RE2 正则，支持 `-i`。搜索根目录依次取 `path`、`scope`、`directory`、`cwd`。
  结果包含完整路径和 1 起始行号。搜索不跟随目录符号链接，并说明跳过的不可读、二进制或超限文件数量。
- 读写仅支持普通文件；读取单文件上限为 4 MiB。更大的文件可通过 shell 处理。

### 命令执行

`run_in_background=true` 启动后立即返回稳定的 `shell_id`，也接受参数 `background`。
轮询和终止使用 `shell_id`，也接受 `commandId`。输出累计保存在客户端进程中，轮询不会清空输出。
每条命令的 stdout 和 stderr 分别最多保存 4 MiB，超限时标记截断。已结束的命令仍可轮询。

命令支持可选 `env`，HOME 始终使用客户端选定的可写目录。
帧中的 `timeout` 单位为毫秒，默认 120 秒；`args.timeout` 可缩短执行期限。
超时终止命令进程组并返回失败，后台命令同样受执行期限约束。
执行预算预留最多 100ms 用于应答传输，小预算预留 10%。

后台命令跨网关重连保留。客户端进程退出后，原 shell_id 失效。
客户端同时最多执行 32 个工具请求，超限立即返回 busy。

### 结果格式

工具应答包含 `executionTimeMs`。命令 `content` 包含 shell_id、退出码、running 和输出正文；
卡片 `state` 使用 `RunCommandState` 或 `GetCommandOutputState` 字段。
文件 `content` 包含路径、正文或错误原因。不支持的工具返回空 content、`success=false` 和明确 error。

异构 Agent、RPC、消息 API 和 HTTP/WebSocket 隧道请求均返回拒绝应答。
浏览器、Computer Use、PTY、沙箱、工作区、JWT / token 刷新和 Magisk 不在支持范围内。
文件和 shell 工具拥有启动客户端的 Android 用户权限。

## 开发检查

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -trimpath -buildvcs=false -o lobe-device .
go run ./scripts/check-elf.go lobe-device
```

宿主机测试使用 `/bin/sh` 执行文件和子进程操作，通过本地 HTTP / WebSocket 服务器检查协议交互。
Android 客户端固定使用 `/system/bin/sh`。
