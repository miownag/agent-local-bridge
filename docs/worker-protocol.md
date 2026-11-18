# Worker HTTP 协议 v2

## 一、连接与鉴权

worker 只主动发送 HTTPS POST，不监听入站端口。所有接口携带：

```http
Authorization: Bearer <BRIDGE_WORKER_TOKEN>
Content-Type: application/json
```

公网只接受 HTTPS。worker 禁止跟随重定向，避免把凭证或任务内容转发到其他目标。本地开发可以显式允许 HTTP 回环地址。

`401` 表示凭证无效，`403` 表示桥接暂停，worker 应停止并让用户处理。`503` 和网络故障可退避重试。请求/响应限制为 128 KiB。

## 二、接口

### 2.1 `POST /worker/heartbeat`

启动时发送，此后约每 10 秒发送：

```json
{"root_name":"workdir","protocol":2}
```

只传默认工作目录 basename 与协议版本，不传完整本地路径。服务端只向 `protocol=2` 的在线 worker 下发工具任务。服务端覆盖时间戳，心跳保留 30 秒。成功返回 `200 {"ok":true}`。

### 2.2 `POST /worker/poll`

请求体为 `{}`。没有任务返回 `204`；有任务返回 `200`：

```json
{
  "id":"opaque-job-id",
  "tool":"read_file",
  "arguments":{"path":"challenge.txt"},
  "deadline":2000000000,
  "receipt":"opaque-result-receipt"
}
```

`deadline` 为 Unix 秒。已过期任务不得执行。receipt 仅用于该任务回传，不能用作 worker 凭证。领取为原子单次交付；不会自动重派，worker 不得自行执行相同任务第二次。

| 工具 | 参数 |
| --- | --- |
| `list_files` | `path`、`offset`、`limit`：目录分页 |
| `read_file` | `path`、`offset`、`limit`、`byte_offset`：按行或字节读取 |
| `write_file` | `path`、`content`、`overwrite`、`expected_sha256`、`create_parents`：创建或校验后覆盖 |
| `edit_file` | `path`、`old_text`、`new_text`、`replace_all`、`expected_sha256`：精确替换 |
| `glob` | `pattern`、`path`、`hidden`、`limit`：文件名搜索 |
| `grep` | `pattern`、`path`、`glob`、`fixed`、`ignore_case`、`hidden`、`limit`：内容搜索 |
| `bash` | `command`、`cwd`、`timeout_seconds`、`yield_ms`：启动命令会话 |
| `bash_poll` | `session_id`、`cursor`、`yield_ms`：增量输出与状态 |
| `bash_list` | 无参数：查询保留会话 |
| `bash_kill` | `session_id`：终止进程组 |

`bridge_status` 由云端读取心跳，不下发给 worker。

### 2.3 `POST /worker/result`

成功：

```json
{"id":"opaque-job-id","receipt":"opaque-result-receipt","output":{"path":"challenge.txt","content":"verification-string"}}
```

失败：

```json
{"id":"opaque-job-id","receipt":"opaque-result-receipt","error":"old_text not found; match whitespace and line endings exactly"}
```

`output` 和 `error` 二选一。output 最大 96 KiB，error 最大 160 字节。工具结果允许包含本地路径与请求内容，事件日志只保留工具和状态。

`200` 表示回执已接受，重复提交保留第一份结果；`404` 表示任务不存在或回执错误，`410` 表示过期，均应停止重试。临时故障可在 deadline 之前重传同一份结果，不能重新执行操作。

## 三、本地权限与资源边界

默认工作目录由 `-cwd` 指定，默认为进程启动目录。`-root` 是同义参数。绝对路径、`~`、隐藏路径、符号链接和 `..` 均可使用，没有目录沙箱。权限等同于 worker 系统用户，Bash 继承进程环境。

文件读取每页最多 12 KiB、默认 200 行。使用 `next_byte_offset` 续读超长单行；可编辑文件的哈希流式计算。读取和编辑仅支持普通 UTF-8 文件，拒绝 FIFO、NUL 和非法 UTF-8。新建或覆盖内容最多 64 KiB，编辑文件最多 8 MiB，精确替换必须唯一或显式 `replace_all`。覆盖文件必须携带 `expected_sha256`，原子替换保留权限位；外部并发写入不具备事务隔离。

Bash 默认超时 120 秒、最多 600 秒，每次等待最多 10 秒，任务 deadline 只约束工具请求，不代表 Shell 会话超时。Shell 在 worker 中继续运行，通过会话工具轮询或终止；不要因工具请求超时而直接重跑命令。最多保留 32 个会话，其中运行中最多 8 个；输出为 256 KiB 环形缓冲，`cursor` 是原始字节位置，`dropped_bytes` 表示已经丢弃的字节。显示文本对非法 UTF-8 使用替换字符。重启 worker 会丢失会话并终止仍受其管理的进程组。

云端请求仍使用一个串行任务槽；异步 Bash 会话不占据该槽。一次部署只运行一个 worker，避免会话查询被另一 worker 领取。升级服务端与 worker 后，需要 Claude 重新取得 `bridge:coding` 授权；旧授权及旧协议不兼容。
