# Agent Local Bridge

通过远程 MCP 连接器，让 Claude 云端访问电脑上的独立测试目录。

## 一、整体设计

```text
Claude 桌面客户端
      │ Anthropic 云端发起 OAuth + MCP 请求
      ▼
Vercel /mcp ─── Redis REST：短期任务、结果、授权状态
      ▲
      │ 本地 worker 主动发送 HTTPS 请求
      │
Mac 上的 bridge-worker ─── 指定测试目录
```

服务端和 worker 使用 Go，MCP 使用官方 `modelcontextprotocol/go-sdk`。Vercel Function 无状态，实例之间通过 Redis REST 共享状态。本地开发可以显式启用内存存储；Vercel 环境禁止使用内存存储。

第一版适用于一个所有者、一个测试目录和一个串行任务槽位。worker 必须在电脑上运行，电脑需要保持联网、唤醒。云端部署本身不能直接读取电脑文件。

| 工具 | 行为 |
| --- | --- |
| `bridge_status` | 查询 worker 是否在线、测试目录名称 |
| `list_files` | 列出相对目录内最多 200 项，根目录传 `.` |
| `read_file` | 读取最多 64 KiB 的 UTF-8 普通文件 |
| `write_file` | 新建最多 64 KiB 的 UTF-8 文件，已有文件不能覆盖 |

文件路径相对于 worker 启动时指定的目录。禁止绝对路径、隐藏路径、`..`、符号链接和特殊文件；文件操作使用 `os.Root` 限定在根目录内。测试目录应为专门创建的普通目录，不应包含挂载点、公司文件或凭证。第一版不提供 shell、文件覆盖、目录创建和文件删除。

## 二、本地运行

### 2.1 构建与验证

需要 Go 1.25 或更新版本。worker 支持 macOS 和 Linux。

```sh
make test
make build
go run ./cmd/setup
mkdir -m 700 test-workspace
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
./bin/bridge-worker -env .env -root ./test-workspace -allow-http
```

`-allow-http` 仅接受回环地址；公网地址始终要求 HTTPS。按 `Ctrl+C` 停止。管理页上的暂停会使 worker 退出；恢复桥接后需要重新启动 worker。

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
./bin/bridge-worker -env worker.env -root ./test-workspace
```

管理页显示在线后再添加 Claude 连接器。

### 3.3 配置 Claude

在 Claude 客户端的连接器设置里添加远程自定义连接器：

- 名称：`Agent Local Bridge`
- URL：`https://your-project.vercel.app/mcp`

服务端提供 OAuth discovery、Dynamic Client Registration、授权码 + S256 PKCE、刷新和撤销。授权页输入所有者密钥后，仅向该客户端授权 `bridge:files`。

正式环境只允许 `https://claude.ai/api/mcp/auth_callback` 作为 OAuth redirect URI。本地开发额外允许 HTTP 回环 callback，供集成测试使用。

连接后在新对话里启用连接器。先在测试目录放一个随机字符串，请 Claude 读取，再让它新建 `hello-from-claude.txt`，最后在 Mac 上检查文件。不要仅以连接器添加成功作为文件访问已成功的证据。

### 3.4 额度与运行时

配置的 Function 上限为 60 秒，单次工具默认等待 45 秒；超时会明确失败。此版本使用 HTTPS 短轮询，不依赖 WebSocket、长驻实例或跨实例内存。

默认每 2 秒轮询，空闲持续运行约产生 43,200 次轮询请求/天，另有心跳与 Redis 命令。Vercel 和 Redis 的免费额度都有限，不能把持续运行等同于永久免费。短期测试完成后停止 worker。可以用 `-poll 5s` 降低轮询频率并接受额外等待；建议保持在 10 秒以内，避免心跳在线状态过期。

## 四、状态、重试与权限

### 4.1 任务语义

任务经过 `pending → running → completed / failed`，等待方还可能记录 `timeout / cancelled`。每次调用生成独立 ID 和结果回执，最多一个任务在途。额外调用返回 busy。

领取任务使用原子 `GETDEL`，同一任务只交付一次。worker 执行后只重试结果回传，不重试文件操作。重复结果使用 `SET NX` 保留第一份结果；过期结果被拒绝。

领取响应丢失或 worker 崩溃时任务可能超时，服务端不会自动重新执行。超时不代表文件操作一定没有发生，重试写入前应检查目标文件。新的调用若使用相同路径，独占新建也会阻止覆盖；本项目不声称端到端 exactly-once。

### 4.2 数据保留

文件参数、文件内容和结果在 Redis 中短期保存，正常调用结束后清理，最长约 90 秒。最近 20 条脱敏事件保留最多 24 小时。管理页和 worker 日志不显示文件内容、完整本地路径或凭证。

OAuth access token 有效期 1 小时，refresh token 单次使用并轮换，最长 7 天；单次授权整体最长约 8 天，之后需要重新授权。Redis 只保存令牌和所有者会话标识的哈希。动态客户端登记有效期 30 天。文件内容仍会经过云端基础设施，目录选择应与实际允许分享给 Claude 的内容一致。

### 4.3 停止与撤销

- 本地停止：对 worker 按 `Ctrl+C`。
- 暂停桥接：管理页点击暂停，停止后续领取；已经领取的操作可能已经执行。
- 撤销 Claude：管理页点击撤销所有授权，access 和 refresh token 随即失效；也可从 Claude 删除连接器。
- 更换 worker 凭证：更换云端和本地的 `BRIDGE_WORKER_TOKEN`，重新部署服务端并重启 worker。
- 清理测试：停止进程后自行删除专用测试目录、`.env` 和 `worker.env`，删除不再使用的 Vercel 项目及 Redis 数据库。

## 五、验证范围

`make test` 验证：真实 HTTP MCP 初始化与工具调用、两个服务端实例共享状态、真实 worker 执行文件读写、OAuth PKCE/授权码单次使用/refresh 轮换/撤销、worker 鉴权、原子领取和回执、离线与超时，以及路径穿越、符号链接、FIFO、超限文件和重复写入拦截。Redis REST 适配器使用本地 Redis 测试服务验证 Lua 脚本和 TTL。

Vercel 正式部署、实际 Upstash 账户和 Claude 免费账号连接需要在对应账户中完成验收，不能由本地测试代替。

## 六、参考

- [Claude 自定义远程连接器](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp)
- [Claude 连接器授权要求](https://claude.com/docs/connectors/building/authentication)
- [官方 Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk)
- [Vercel Go Runtime](https://vercel.com/docs/functions/runtimes/go)

worker HTTP 协议见 [docs/worker-protocol.md](docs/worker-protocol.md)。
