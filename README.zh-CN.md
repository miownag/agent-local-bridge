# Agent Local Bridge

**让 Chat 工具像 Agent 一样工作。**

[English](README.md) · 简体中文

Agent Local Bridge 为云端聊天助手接入本地工具，让它在同一段对话里读取仓库、修改文件、执行命令、检查测试结果，再继续完成任务。

**当前支持 Claude Desktop 的 Chat 模式。** 通过远程自定义连接器接入，在电脑上启动 Go worker，就可以让 Claude 处理本地项目。模型仍运行在 Claude 云端，文件操作与命令由本地 worker 执行。桥接程序不调用模型 API，也不需要模型 API Key。

> “看看这个仓库，修复失败的测试，跑一遍验证，再告诉我改了什么。”

## 一、能做什么

- **按需读取。** 文件按页返回，限制输出长度，超长单行也能按字节继续读取。
- **精确修改。** 精确文本替换、重复匹配拒绝、SHA256 陈旧内容检查、已有文件原子替换。
- **执行真实命令。** Bash 会话支持工作目录、超时、退出码、输出限制，以及查询和终止。
- **查找代码。** 文件名和内容搜索保留 `.gitignore` 规则，并限制结果数量。
- **连接内网电脑。** worker 主动发起出站 HTTPS 请求，电脑无需公网 IP 或开放入站端口。

### 1.1 支持范围

| 客户端 / 模式 | 状态 |
| --- | --- |
| Claude Desktop → Chat → 远程自定义连接器 | 支持 |
| Claude Desktop → Code、Cowork | 本指南不支持，当前版本未验证 |
| Claude 网页端、移动端 | 当前版本未验证 |
| 其他 MCP 客户端 | 需要接入适配及授权配置调整 |

服务端采用标准 **MCP Streamable HTTP**，但正式环境的 OAuth 回调目前只允许 Claude。客户端“支持 MCP”或具有名为“connector”的功能，并不代表能直接接入。当前不提供 stdio 或旧式 HTTP+SSE 接口。

## 二、工作原理

```text
Claude Desktop · Chat
        │ Claude 云端发起 OAuth + MCP 请求
        ▼
Vercel 公网 HTTPS 服务 ── Redis REST
        ▲                 任务、结果、授权状态
        │ 主动发送出站 HTTPS 轮询
        │
本地 Go worker ── 文件、代码仓库、Bash 进程
```

服务端与 worker 均使用 Go。服务端使用官方 Go MCP SDK，通过 Redis REST 共享状态，Vercel 实例无需常驻内存或 WebSocket。

一个部署对应**一个所有者、一个 worker**，同一时刻处理一个工具请求。Bash 会话可以在两次请求之间继续运行。电脑需要保持唤醒、联网，worker 需要持续运行。

**本地权限等同于启动 worker 的系统用户。** 支持绝对路径、`~`、隐藏文件、符号链接和 `..`。`-cwd` 只指定默认工作目录，不是目录沙箱。Bash 继承 worker 的环境变量并可访问网络。文件内容和命令输出会经过桥接基础设施和 Claude。

## 三、开始使用

构建需要 Go **1.25+**；worker 支持 macOS 和 Linux，需要 `/bin/bash`，代码搜索需要 PATH 中的 **ripgrep（`rg`）**。目标电脑运行编译好的 worker 不需要安装 Go。

还需要 Vercel 项目、支持 Redis REST 的数据库（例如 Upstash），以及 Claude Desktop 中的远程自定义连接器入口。连接 Claude 云端需要公网 HTTPS 服务，不能填写 localhost 地址。

### 3.1 构建并生成密钥

```sh
git clone https://github.com/miownag/agent-local-bridge.git
cd agent-local-bridge
make build
go run ./cmd/setup -out .env.local
```

如果没有安装 ripgrep，macOS 可执行 `brew install ripgrep`。

初始化命令生成已被 Git 忽略的文件，权限仅允许所有者访问，其中包含两份不同的随机密钥。命令不会覆盖已有文件，也不会打印密钥值。

| 生成值 | 用途 |
| --- | --- |
| `BRIDGE_ADMIN_SECRET` | 所有者密钥：登录桥接管理页、授权 Claude |
| `BRIDGE_WORKER_TOKEN` | worker 凭证：服务端和本机配置相同的值 |

**授权页面要求的“所有者密钥”，就是 `.env.local` 中 `BRIDGE_ADMIN_SECRET` 的值**，不要填写 worker token。已有部署应使用其现有密钥。

### 3.2 部署服务端

将仓库导入 Vercel，框架预设选择 **Other**，使用仓库自带的 `vercel.json`，不设置前端构建参数。Go Function 入口为 `api/index.go`。

配置以下正式环境变量：

| 变量 | 值 |
| --- | --- |
| `BRIDGE_PUBLIC_URL` | 固定的 HTTPS origin，例如 `https://your-project.vercel.app`，不带路径 |
| `BRIDGE_ADMIN_SECRET` | 上一步生成的所有者密钥 |
| `BRIDGE_WORKER_TOKEN` | 上一步生成的 worker 凭证 |
| `BRIDGE_NAMESPACE` | 独立存储前缀，例如 `agent-local-bridge-production` |
| `UPSTASH_REDIS_REST_URL` | 数据库的 HTTPS REST URL |
| `UPSTASH_REDIS_REST_TOKEN` | 数据库的读写 REST token |

通过 Vercel Marketplace 连接 Upstash 时，也支持自动注入的 `KV_REST_API_URL` 和 `KV_REST_API_TOKEN`。显式设置的 `UPSTASH_REDIS_REST_*` 优先；不能混用两组的 URL 和 token。

生成的 `.env.local` 包含本地开发默认值，**不要整体导入正式环境，也不要在 Vercel 设置 `BRIDGE_DEV`**。复制两份密钥，单独设置公网 URL、namespace 和 Redis。Preview 与 Production 使用不同的凭证、namespace 和 origin。

设置变量后部署。访问 `https://your-project.vercel.app/healthz`，应返回 `{"status":"ok"}`；再打开根地址，使用所有者密钥登录管理页。健康检查只确认初始化，不检查 Redis 连通性；管理页会实际访问 Redis。

MCP、OAuth、worker 接口必须能从公网访问。如果 Vercel Deployment Protection 把请求拦截到登录页，需要调整部署访问方式，让 Claude 和 worker 能访问应用。保留桥接程序自身的鉴权。

### 3.3 启动本地 worker

在电脑上创建已被 Git 忽略的 `.env.worker`：

```dotenv
BRIDGE_PUBLIC_URL=https://your-project.vercel.app
BRIDGE_WORKER_TOKEN=与服务端相同的worker凭证
```

```sh
chmod 600 .env.worker
./bin/bridge-worker -env .env.worker -cwd /absolute/path/to/your/project -poll 5s
```

将项目路径替换为实际存在的目录。管理页应显示 worker 在线。不传 `-cwd` 时使用当前目录；`-root` 是兼容同义参数，同样不限制目录访问。

### 3.4 连接 Claude Desktop Chat

1. 在 Claude Desktop 打开 **Settings → Customize → Connectors**，添加远程自定义连接器。
2. 名称可填写 **Agent Local Bridge**，URL 填写 `https://your-project.vercel.app/mcp`。
3. 在浏览器中使用 **`BRIDGE_ADMIN_SECRET`** 完成授权。`bridge:coding` 权限允许通过 worker 进行本地文件操作和执行 Bash。
4. 返回 Claude，开启 Chat 对话，在该对话的工具菜单中启用连接器。

看到 **Connection has expired** 时，点击 **Reconnect**，重新完成授权。旧 `bridge:files` 授权不能访问编码工具；升级服务端与 worker 到协议 v2 后需要重新连接。正式环境的 OAuth 回调只允许 `https://claude.ai/api/mcp/auth_callback`。

### 3.5 验证并开始处理项目

先在本地创建一个空目录做小测试，将下面路径替换为它的绝对路径：

```text
请使用 Agent Local Bridge 检查 bridge_status。
然后在 /absolute/path/to/bridge-demo 中：
1. 创建 hello.txt，内容为 "hello bridge"。
2. 读取文件，用 edit_file 将 "hello bridge" 改成 "hello agent"。
3. 用 bash 在该目录执行 cat hello.txt。
请报告工具真实返回的结果，工具失败时报告错误。
```

同时在本机检查文件。确认链路可用后，再让 Claude 读取仓库、修改代码、执行测试并汇报结果。例如：

```text
请使用 Agent Local Bridge 处理 /absolute/path/to/my-project。
先阅读项目说明，定位失败的测试，修复原因，运行相关测试，
最后说明修改了哪些文件，以及测试结果。
```

## 四、工具与运行限制

| 工具 | 行为 |
| --- | --- |
| `bridge_status` | worker 在线状态、默认目录名称、协议版本 |
| `list_files` | 包含隐藏文件的目录分页，默认 200 项，使用 `next_offset` 继续 |
| `read_file` | UTF-8 文本分页，默认 200 行，每页最多 12 KiB，支持行偏移和字节偏移 |
| `write_file` | 新建文件，可选创建父目录；覆盖需要 `overwrite=true` 和 `expected_sha256` |
| `edit_file` | 精确替换，默认唯一匹配，可选 `replace_all` 和 `expected_sha256` |
| `glob` / `grep` | 文件名 / 内容搜索，默认 100 项，最多 1000 项、12 KiB，10 秒超时 |
| `bash` | 非交互 `/bin/bash -c`；未完成时返回会话 ID |
| `bash_poll` / `bash_list` / `bash_kill` | 续取输出、查找保留会话、终止进程组 |

**读取与编辑：** `offset` 是从 1 开始的行号，`limit` 最大 2000。读取被截断时，使用 `next_byte_offset` 继续，尤其是 `ends_mid_line=true` 时。字节偏移不与大于 1 的行偏移混用。行扫描最多 64 MiB，更远位置使用字节偏移。8 MiB 以内文件返回 SHA256，供陈旧内容检查使用。读取和编辑拒绝特殊文件、NUL 字节和非法 UTF-8。单次写入或编辑文本最多 64 KiB，可编辑文件最多 8 MiB。精确编辑保留 CRLF；原子替换保留权限位，但不提供与外部写入进程之间的事务隔离。

**搜索：** 保留忽略规则，默认跳过隐藏文件，可用 `hidden=true` 开启；不递归跟随目录符号链接。发现文件最多 10000 个候选或 1 MiB 路径。结果截断时应缩小路径或匹配条件；搜索没有稳定分页游标。

**Bash：** 默认超时 120 秒，最多 600 秒。单次工具请求最多等待 10 秒，命令可在本地会话中继续。用 `session_id` 和 `next_cursor` 轮询，直到 `running=false` 且 `has_more=false`。stdout/stderr 合并，每次最多返回 12 KiB，只保留最新 256 KiB；`dropped_bytes` 表示丢失的输出，大日志应重定向到文件。最多 8 个运行会话、32 个保留会话，完成约 15 分钟后可被清理。worker 重启不保留会话。超时、终止或停止 worker 会杀死进程组；主动脱离进程组的进程不在该清理范围内。stdin 关闭，不支持交互终端。

## 五、运行与维护

- **停止：** 在 worker 终端按 `Ctrl+C`，同时终止其管理的 Bash 进程组。
- **暂停或撤销：** 使用桥接管理页。暂停会阻止后续任务领取，并使 worker 退出；恢复后需重新启动 worker。撤销会使 Claude 授权失效。两种操作均不会撤回已经执行的操作。
- **处理超时：** 重试前检查文件，或使用 `bash_list` 查找会话。任务单次领取，结果允许重传，操作不会自动重跑；超时不代表没有执行。
- **关注用量：** 默认每 2 秒轮询，持续运行约产生 43200 次轮询/天，另有心跳和 Redis 流量。`-poll 5s` 用额外延迟换取更少流量；建议不超过 10 秒。免费托管方案有额度限制，不保证持续运行始终免费。
- **数据保留：** 任务参数和结果在 Redis 中短期保存，正常调用结束后删除，最长约 90 秒。最近 20 条仅含元数据的事件最多保留 24 小时。access token 有效期 1 小时，refresh token 单次使用并轮换、最长 7 天，单次授权整体约 8 天。

Vercel Function 上限配置为 60 秒，单次工具请求默认在服务端等待 45 秒。长 Bash 命令运行在 worker 上，不占用 Function 持续执行。每个部署只运行一个 worker，确保会话查询能到达持有会话的电脑。

## 六、本地开发

前面生成的密钥文件也可以启动仅回环访问、使用内存存储的开发服务端：

```sh
./bin/bridge-server -env .env.local
```

在另一个终端中，从仓库目录启动 worker：

```sh
./bin/bridge-worker -env .env.local -cwd /absolute/path/to/your/project -allow-http
```

管理页地址为 `http://127.0.0.1:8787`。此模式用于本地开发和集成测试；Claude 云端仍需要公网 HTTPS 部署。`-allow-http` 只允许回环地址，正式环境必须使用持久化 Redis REST 存储。

`make test` 执行带竞态检查的 Go 测试和 vet，覆盖 HTTP MCP → worker、跨服务实例共享状态、OAuth、旧授权失效、分页、编辑、特殊文件拒绝、搜索上限和 Bash 生命周期。本地测试不能代替客户端实际验收。

交叉编译 worker，例如：

```sh
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o bin/bridge-worker-macos-arm64 ./cmd/worker
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/bridge-worker-linux-amd64 ./cmd/worker
```

自行构建的 macOS 二进制未经过 Apple 公证，分发时可能需要按正常系统提示允许打开。

## 七、参考

- [Worker HTTP 协议 v2](docs/worker-protocol.md)
- [环境变量模板](.env.example)
- [官方 Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk)
- [Claude 远程自定义连接器](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp)
- [Vercel Go Runtime](https://vercel.com/docs/functions/runtimes/go)
