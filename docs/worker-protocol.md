# Worker HTTP 协议 v1

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
{"root_name":"test-workspace"}
```

只传目录 basename，不传完整本地路径。服务端覆盖时间戳，心跳保留 30 秒。成功返回 `200 {"ok":true}`。

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
| `list_files` | `path`：相对目录；`.` 为根目录 |
| `read_file` | `path`：相对文件路径 |
| `write_file` | `path`、`content`：相对新文件路径及 UTF-8 内容 |

`bridge_status` 由云端读取心跳，不下发给 worker。

### 2.3 `POST /worker/result`

成功：

```json
{"id":"opaque-job-id","receipt":"opaque-result-receipt","output":{"path":"challenge.txt","content":"verification-string"}}
```

失败：

```json
{"id":"opaque-job-id","receipt":"opaque-result-receipt","error":"symbolic links are not allowed"}
```

`output` 和 `error` 二选一。output 最大 96 KiB，error 最大 160 字节，错误不得包含完整路径或文件内容。

`200` 表示回执已接受，重复提交保留第一份结果；`404` 表示任务不存在或回执错误，`410` 表示过期，均应停止重试。临时故障可在 deadline 之前重传同一份结果，不能重新执行操作。

## 三、文件边界

根目录由用户启动 worker 时指定。worker 独立执行路径检查，不依赖云端过滤。拒绝绝对路径、`..`、隐藏路径、符号链接、特殊文件和超限内容。文件读取和新建仅支持 UTF-8。写入使用独占创建，权限为 `0600`。

本地根目录使用 `os.Root` 打开，防止目录或符号链接在检查后变化导致越界。专用测试目录不应包含挂载点。一个部署对应一个凭证和一个测试目录；多个电脑应使用独立部署或独立 namespace，不能混用同一 worker 凭证。
