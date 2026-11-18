# Agent Local Bridge

通过远程 MCP 连接器，让 Claude 云端读写本地文件、搜索代码和执行命令。服务端与本地 worker 均使用 Go。

## 一、整体设计

```text
Claude 桌面客户端
      │ Anthropic 云端发起 OAuth + MCP 请求
      ▼
Vercel /mcp ─── Redis REST：短期任务、结果、授权状态
      ▲
      │ 本地 worker 主动发送 HTTPS 请求
      │
Mac 上的 bridge-worker ─── 本地文件系统 + Bash 会话
```

服务端和 worker 使用 Go，MCP 使用官方 `modelcontextprotocol/go-sdk`。Vercel Function 无状态，实例之间通过 Redis REST 共享状态。本地开发可以显式启用内存存储；Vercel 环境禁止使用内存存储。

一个部署对应一个所有者、一个 worker 和一个串行请求槽位；Bash 命令可以在本地异步运行。worker 必须在电脑上运行，电脑需要保持联网、唤醒。云端部署本身不能直接读取电脑文件。

| 工具 | 行为 |
| --- | --- |
| `bridge_status` | 查询 worker 在线状态、默认目录名称与协议版本 |
| `list_files` | 包含隐藏文件的目录分页，默认 200 项，使用 `next_offset` 继续 |
| `read_file` | UTF-8 文本分页；默认最多 200 行、12 KiB，支持行偏移与字节续读 |
| `write_file` | 新建 UTF-8 文件、可选创建父目录；覆盖需要 `overwrite` 和读取时的 SHA256 |
| `edit_file` | 精确文本替换；默认唯一匹配，可选择全部替换和 SHA256 陈旧检查 |
| `glob` / `grep` | 文件名和内容搜索，尊重 `.gitignore`，分页上限和超时保护 |
| `bash` | 非交互 Bash，独立 cwd、超时、退出码，长命令返回会话 ID |
| `bash_poll` / `bash_list` / `bash_kill` | 查询输出、找回命令会话、终止进程组 |

路径支持绝对路径、`~`、隐藏文件、符号链接和 `..`。相对路径基于 worker 的 `-cwd`。此目录是默认工作目录，不是沙箱；所有工具拥有启动 worker 的系统用户权限。Bash 继承其环境变量并可访问网络。macOS 系统权限仍由系统控制。

### 1.1 读取与编辑

`read_file(path, offset=1, limit=200)` 默认最多返回 12 KiB 文本。`offset` 为从 1 开始的行号；`limit` 最大 2000。返回 `truncated=true` 时使用 `next_byte_offset` 继续，尤其适用于压缩 JSON 等超长单行；`ends_mid_line=true` 时不能只靠 `next_line` 续读。`byte_offset` 不与行偏移混用。行偏移扫描最多 64 MiB，更远位置使用字节偏移。读取使用有界缓冲，拒绝 NUL 和非法 UTF-8，不把整个大文件装入内存。

8 MiB 以内文件额外返回流式计算的 `sha256`。编辑前先读文件，将哈希作为 `expected_sha256` 传入，避免覆盖读取后发生的变化。`edit_file` 的 `old_text` 必须精确匹配空白和换行；重复出现时需要增加上下文或显式 `replace_all=true`。替换保留 CRLF 和权限位，使用同目录临时文件、同步后重命名，返回简短预览、首个修改行和新哈希。覆盖写入同样使用原子替换。文件操作会检查并发变化，但不提供与外部进程之间的事务锁；不要同时让多个进程改写同一文件。

单次写入内容或编辑文本合计最多 64 KiB，可编辑文件最多 8 MiB；更大的批处理使用 Bash。`write_file` 默认独占创建，`overwrite=true` 必须附带读取时的 SHA256。新文件权限为 `0600`，`create_parents=true` 创建缺失父目录。

### 1.2 搜索与命令

`glob` 使用 `**/*.go` 等 glob；`grep` 支持正则、字面量、大小写开关和文件名过滤。依赖 worker PATH 中的 `rg`，Mac 可以安装 `brew install ripgrep`。默认跳过隐藏文件，传 `hidden=true` 开启，仍尊重忽略规则；不递归跟随目录符号链接。最多返回 1000 项、12 KiB，默认 100 项，10 秒超时。文件发现阶段最多 10000 个候选文件或 1 MiB 路径，达到上限明确标记 `truncated`，应缩小目录或过滤条件。搜索无稳定分页游标。

`bash(command, cwd, timeout_seconds=120, yield_ms=1000)` 执行 `/bin/bash -c`，`cwd` 不会改变后续调用的默认目录。超时最多 600 秒，单次等待最多 10 秒；未完成时返回 `session_id`，用 `bash_poll(session_id, cursor=next_cursor)` 续取输出，直到 `running=false` 且 `has_more=false`。`exit_code` 为完成后的退出码，非零不隐藏输出。输入关闭，不支持交互终端。

stdout/stderr 合并，每次最多返回 12 KiB，内存仅保留最新 256 KiB；输出落后时 `dropped_bytes` 明确标记丢失量，大日志应重定向到文件再分页读取。最多 8 个运行会话、32 个保留会话，完成会话约 15 分钟后可被回收，worker 重启会丢失会话。请求超时后先 `bash_list` 查找会话，避免重复运行命令。超时、终止或停止 worker 会杀死进程组；主 Shell 退出后清理同组后台子进程。主动脱离进程组的进程不在此清理范围内。

## 二、本地运行

### 2.1 构建与验证

需要 Go 1.25 或更新版本。worker 支持 macOS 和 Linux；代码搜索需要 ripgrep。

```sh
make test
make build
go run ./cmd/setup
mkdir -p workdir
```

`cmd/setup` 生成 `.env`，包含独立的随机所有者密钥和 worker 凭证，权限为 `0600`。不会覆盖已有文件，也不会输出密钥。`.env` 已被 Git 忽略。

### 2.2 启动服务端

```sh
./bin/bridge-server -env .env
```

打开 <http://127.0.0.1:8787>，使用 `.env` 中的 `BRIDGE_ADMIN_SECRET` 登录管理页。

### 2.3 启动本地 worker

另开一个终端：

```sh
./bin/bridge-worker -env .env -cwd ./workdir -allow-http
```

`-cwd` 默认为启动目录，`-root` 保留为同义参数，不限制文件访问范围。`-allow-http` 仅接受回环地址；公网地址始终要求 HTTPS。按 `Ctrl+C` 停止。管理页上的暂停会使 worker 退出；恢复桥接后需要重新启动 worker。

本地 HTTP 模式用于开发和集成测试。Claude 远程连接器从云端访问，需要第三节的公网 HTTPS 部署。

### 2.4 编译分发

```sh
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o bin/bridge-worker-macos-arm64 ./cmd/worker
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -o bin/bridge-worker-macos-amd64 ./cmd/worker
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/bridge-worker-linux-amd64 ./cmd/worker
```

目标电脑运行 worker 不需要安装 Go。自行编译的 macOS 程序没有 Apple 公证，跨机器分发时可能需要按系统提示允许打开。

## 三、部署到 Vercel

### 3.1 资源与环境变量

在 Vercel 中导入仓库，Framework Preset 选择 **Other**，使用仓库的 `vercel.json`，不要配置前端构建命令。Go Function 入口为 `api/index.go`。

准备支持 Redis REST API 的 Upstash 数据库，设置以下服务端环境变量：

| 变量 | 含义 |
| --- | --- |
| `BRIDGE_PUBLIC_URL` | 正式部署的固定 HTTPS origin，例如 `https://your-project.vercel.app`，不带路径 |
| `BRIDGE_ADMIN_SECRET` | 至少 32 字节的随机所有者密钥 |
| `BRIDGE_WORKER_TOKEN` | 不同于所有者密钥的随机 worker 凭证，至少 32 字节 |
| `BRIDGE_NAMESPACE` | 此部署专用的存储前缀，例如 `bridge-production` |
| `UPSTASH_REDIS_REST_URL` | Redis REST HTTPS URL |
| `UPSTASH_REDIS_REST_TOKEN` | 具备读写能力的 Redis REST token |

通过 Vercel Marketplace 连接 Upstash 时，也支持自动注入的 `KV_REST_API_URL` 和 `KV_REST_API_TOKEN`，无需复制或重命名。显式配置的 `UPSTASH_REDIS_REST_*` 优先，不能混用两组中的 URL 和 token。

可以将 `cmd/setup` 生成的两个随机值分别复制到 Vercel 环境变量。不要把 `.env` 提交到仓库。Vercel 上不要设置 `BRIDGE_DEV`。Preview 和 Production 应使用不同的 namespace、凭证和 origin。

正式 MCP endpoint、OAuth metadata 和 worker API 必须能从公网访问。如果启用了 Vercel Deployment Protection，需要为目标部署配置适当的访问方式，否则 Anthropic 和 worker 会被 Vercel 登录页拦截。应用自己的 OAuth 和 worker 鉴权必须保留。

配置变化后重新部署。先验证 `GET /healthz` 返回 `{"status":"ok"}`；该接口仅表示配置初始化成功，不检查 Redis 连通性。管理页和 OAuth 操作会实际访问 Redis。

### 3.2 配置 Mac

在本地创建权限为 `0600` 的 `worker.env`，只需要两个变量：

```dotenv
BRIDGE_PUBLIC_URL=https://your-project.vercel.app
BRIDGE_WORKER_TOKEN=与云端配置相同的worker凭证
```

```sh
chmod 600 worker.env
./bin/bridge-worker -env worker.env -cwd ./workdir
```

管理页显示在线后再添加 Claude 连接器。

### 3.3 配置 Claude

在 Claude 客户端的连接器设置里添加远程自定义连接器：

- 名称：`Agent Local Bridge`
- URL：`https://your-project.vercel.app/mcp`

服务端提供 OAuth discovery、Dynamic Client Registration、授权码 + S256 PKCE、刷新和撤销。授权页输入所有者密钥后，向该客户端授权 `bridge:coding`。授权页明确包含文件修改、Bash 和完整用户权限。旧 `bridge:files` 令牌、刷新令牌及尚未完成的旧授权不能升级，需要在 Claude 中重新连接授权。服务端要求协议 v2 worker，旧二进制会提示升级。

正式环境只允许 `https://claude.ai/api/mcp/auth_callback` 作为 OAuth redirect URI。本地开发额外允许 HTTP 回环 callback，供集成测试使用。

连接后在新对话里启用连接器。先在工作目录放一个随机字符串，请 Claude 读取，再让它新建 `hello-from-claude.txt`，最后在 Mac 上检查文件。不要仅以连接器添加成功作为文件访问已成功的证据。

### 3.4 额度与运行时

配置的 Function 上限为 60 秒，单次工具默认等待 45 秒；超时会明确失败。此版本使用 HTTPS 短轮询，不依赖 WebSocket、长驻实例或跨实例内存。

默认每 2 秒轮询，空闲持续运行约产生 43,200 次轮询请求/天，另有心跳与 Redis 命令。Vercel 和 Redis 的免费额度都有限，不能把持续运行等同于永久免费。短期测试完成后停止 worker。可以用 `-poll 5s` 降低轮询频率并接受额外等待；建议保持在 10 秒以内，避免心跳在线状态过期。

## 四、状态、重试与权限

### 4.1 任务语义

任务经过 `pending → running → completed / failed`，等待方还可能记录 `timeout / cancelled`。每次调用生成独立 ID 和结果回执，最多一个任务在途。额外调用返回 busy。

领取任务使用原子 `GETDEL`，同一任务只交付一次。worker 执行后只重试结果回传，不重试文件操作。重复结果使用 `SET NX` 保留第一份结果；过期结果被拒绝。

领取响应丢失或 worker 崩溃时任务可能超时，服务端不会自动重新执行。超时不代表文件操作一定没有发生，重试写入前应检查目标文件，重试命令前用 `bash_list` 查找已运行的会话。本项目不声称端到端 exactly-once。

### 4.2 数据保留

文件参数、文件内容和结果在 Redis 中短期保存，正常调用结束后清理，最长约 90 秒。最近 20 条脱敏事件保留最多 24 小时。管理页和 worker 日志不显示文件内容、完整本地路径或凭证。

OAuth access token 有效期 1 小时，refresh token 单次使用并轮换，最长 7 天；单次授权整体最长约 8 天，之后需要重新授权。Redis 只保存令牌和所有者会话标识的哈希。动态客户端登记有效期 30 天。文件内容仍会经过云端基础设施，完整目录权限允许 Claude 请求其他目录内容；只连接自己信任的客户端。

### 4.3 停止与撤销

- 本地停止：对 worker 按 `Ctrl+C`，同时终止运行中的 Bash 进程组。
- 暂停桥接：管理页点击暂停，停止后续领取；已经领取的操作可能已经执行。
- 撤销 Claude：管理页点击撤销所有授权，access 和 refresh token 随即失效；也可从 Claude 删除连接器。
- 更换 worker 凭证：更换云端和本地的 `BRIDGE_WORKER_TOKEN`，重新部署服务端并重启 worker。
- 清理测试：停止进程后自行删除测试生成的文件、`.env` 和 `worker.env`，删除不再使用的 Vercel 项目及 Redis 数据库。

## 五、验证范围

`make test` 验证真实 HTTP MCP 与 worker 的读写、编辑和 Shell 调用，跨服务实例状态共享、OAuth PKCE、令牌轮换与撤销、旧 scope 失效、旧 worker 拒绝，以及中文长行分页、二进制/FIFO 拒绝、精确替换歧义、陈旧哈希、CRLF、符号链接目标、权限保留、命令退出/超时/终止、输出截断和忽略规则。Redis REST 适配器使用本地 Redis 测试服务验证 Lua 和 TTL。

本地集成测试不代替 Vercel、实际 Redis 和 Claude 客户端中的部署验收。

## 六、参考

- [Claude 自定义远程连接器](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp)
- [Claude 连接器授权要求](https://claude.com/docs/connectors/building/authentication)
- [官方 Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk)
- [Vercel Go Runtime](https://vercel.com/docs/functions/runtimes/go)

worker HTTP 协议见 [docs/worker-protocol.md](docs/worker-protocol.md)。
