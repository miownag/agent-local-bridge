# Agent Local Bridge

**Let your chat app work like an agent.**

English · [简体中文](README.zh-CN.md)

Agent Local Bridge gives a cloud-hosted chat assistant tools to work on your computer: read a repository, edit files, run commands, inspect test results, and continue the task in the same conversation.

**Currently supported: Claude Desktop in Chat mode.** Connect it through a remote custom connector, run the Go worker on your computer, and ask Claude to work with your local project. The model runs in Claude's cloud; files and commands are handled by your local worker. The bridge does not call a model API or require a model API key.

> “Inspect this repository, fix the failing test, run the tests, and show me what changed.”

## What it does

- **Read without flooding the conversation.** Files are returned in bounded pages, including byte-based continuation for very long lines.
- **Edit precisely.** Exact text replacement, ambiguous-match rejection, SHA256 checks for stale content, and atomic replacement of existing files.
- **Run real commands.** Bash sessions have a working directory, timeout, exit code, bounded output, and tools to poll or stop them.
- **Find code.** File globs and content search respect `.gitignore` and cap their results.
- **Connect from behind NAT.** The worker makes outbound HTTPS requests. Your computer needs no public IP or inbound port.

### Supported clients

| Client / mode | Status |
| --- | --- |
| Claude Desktop → Chat → remote custom connector | Supported |
| Claude Desktop → Code or Cowork | Not supported by this guide or validated in this release |
| Claude in a browser or on mobile | Not validated in this release |
| Other MCP clients | Requires client integration and authorization changes |

The server implements standard **MCP Streamable HTTP**, but production OAuth currently allows only Claude's callback URL. Supporting MCP or having a feature named “connector” is not enough to connect another client directly. There is no stdio or legacy HTTP+SSE endpoint.

## How it works

```text
Claude Desktop · Chat
        │ Claude cloud sends OAuth + MCP requests
        ▼
Public HTTPS server on Vercel ── Redis REST
        ▲                       jobs, results, authorization
        │ outbound HTTPS polling
        │
Local Go worker ── your files, repositories, and Bash processes
```

Both the server and worker use Go. The server uses the official Go MCP SDK and shares state through Redis REST, so Vercel instances do not need persistent memory or WebSockets.

One deployment serves **one owner and one worker**, with one tool request in flight at a time. Bash sessions can continue locally between requests. Keep the computer awake, online, and the worker running.

**Local access follows the worker's OS user permissions.** Absolute paths, `~`, hidden files, symlinks, and `..` are supported. `-cwd` selects the default working directory; it is not a directory sandbox. Bash inherits the worker environment and can access the network. File contents and command output pass through the bridge infrastructure and Claude.

## Get started

You need Go **1.25+** to build, macOS or Linux for the worker, `/bin/bash`, and **ripgrep (`rg`)** on the worker's PATH for code search. The compiled worker does not need Go installed on the target computer.

You also need a Vercel project, a Redis REST database such as Upstash, and access to remote custom connectors in Claude Desktop. This setup uses a public HTTPS server; a localhost URL cannot be reached by Claude's cloud.

### 1. Build and generate credentials

```sh
git clone https://github.com/miownag/agent-local-bridge.git
cd agent-local-bridge
make build
go run ./cmd/setup -out .env.local
```

Install ripgrep if it is missing, for example `brew install ripgrep` on macOS.

The setup command creates a gitignored file with owner-only permissions and two distinct random secrets. It refuses to overwrite an existing file and does not print secret values.

| Generated value | Use |
| --- | --- |
| `BRIDGE_ADMIN_SECRET` | Owner key: log in to the bridge dashboard and authorize Claude |
| `BRIDGE_WORKER_TOKEN` | Worker credential: configure the same value on the server and your computer |

The **owner key requested by the authorization page is the value of `BRIDGE_ADMIN_SECRET` in `.env.local`**, not the worker token. For an existing deployment, use its existing credentials.

### 2. Deploy the server

Import this repository into Vercel. Choose **Other** as the framework preset, use the included `vercel.json`, and leave frontend build settings unset. The Go Function entry point is `api/index.go`.

Configure these production environment variables:

| Variable | Value |
| --- | --- |
| `BRIDGE_PUBLIC_URL` | Your fixed HTTPS origin, e.g. `https://your-project.vercel.app`, without a path |
| `BRIDGE_ADMIN_SECRET` | Owner key generated in step 1 |
| `BRIDGE_WORKER_TOKEN` | Worker credential generated in step 1 |
| `BRIDGE_NAMESPACE` | A unique storage prefix, e.g. `agent-local-bridge-production` |
| `UPSTASH_REDIS_REST_URL` | Your database's HTTPS REST URL |
| `UPSTASH_REDIS_REST_TOKEN` | Its read/write REST token |

A Vercel Marketplace Upstash integration can supply `KV_REST_API_URL` and `KV_REST_API_TOKEN` instead. Explicit `UPSTASH_REDIS_REST_*` values take priority; do not mix a URL from one pair with a token from the other.

The generated `.env.local` contains local development defaults. **Do not import it wholesale into production or set `BRIDGE_DEV` on Vercel.** Copy the two secrets, set the public URL and namespace, and configure Redis separately. Use distinct credentials, namespaces, and origins for Preview and Production.

Deploy after setting the variables. Check `https://your-project.vercel.app/healthz` for `{"status":"ok"}`, then open the root URL and log in with the owner key. The health endpoint checks initialization, not Redis connectivity; the dashboard accesses Redis.

The MCP, OAuth, and worker endpoints must be publicly reachable. If Vercel Deployment Protection intercepts them with a login page, configure deployment access so Claude and the worker can reach the app. Keep the bridge's own authentication enabled.

### 3. Start the local worker

Create a gitignored `.env.worker` file on your computer:

```dotenv
BRIDGE_PUBLIC_URL=https://your-project.vercel.app
BRIDGE_WORKER_TOKEN=the-same-worker-credential-as-the-server
```

```sh
chmod 600 .env.worker
./bin/bridge-worker -env .env.worker -cwd /absolute/path/to/your/project -poll 5s
```

Replace the project path with an existing directory. The dashboard should show the worker online. Omit `-cwd` to use the current directory; `-root` is a compatibility alias with the same unrestricted access.

### 4. Connect Claude Desktop Chat

1. Open **Settings → Customize → Connectors** in Claude Desktop. Add a custom remote connector.
2. Use a name such as **Agent Local Bridge** and the URL `https://your-project.vercel.app/mcp`.
3. Complete the browser authorization using your **`BRIDGE_ADMIN_SECRET`**. The `bridge:coding` scope grants local file operations and Bash access through the worker.
4. Return to Claude, start a Chat conversation, and enable the connector in that conversation's tools menu.

For **Connection has expired**, choose **Reconnect** and complete authorization again. Old `bridge:files` grants cannot access the coding tools; reconnect after upgrading both server and worker to protocol v2. Production OAuth accepts only `https://claude.ai/api/mcp/auth_callback` as its redirect URL.

### 5. Verify, then work on a project

Start with a small test in an empty directory you create locally. Replace the path below with its absolute path:

```text
Use Agent Local Bridge to check bridge_status.
In /absolute/path/to/bridge-demo:
1. Create hello.txt containing "hello bridge".
2. Read it and use edit_file to replace "hello bridge" with "hello agent".
3. Use bash in that directory to run cat hello.txt.
Report the actual tool results. If a tool fails, report the error.
```

Check the file locally as well. Once the round trip works, ask Claude to inspect a repository, make a change, run tests, and summarize the result. An example task:

```text
Use Agent Local Bridge to work in /absolute/path/to/my-project.
Read the project instructions, locate the failing test, fix the cause,
run the relevant tests, and report the files changed and test results.
```

## Tools and operating limits

| Tool | Behavior |
| --- | --- |
| `bridge_status` | Worker availability, default directory name, and protocol version |
| `list_files` | Directory entries including hidden files; default 200 entries, continue with `next_offset` |
| `read_file` | UTF-8 text; default 200 lines, at most 12 KiB per page; line or byte offsets |
| `write_file` | Create a file, optionally create parents; overwrite requires `overwrite=true` and `expected_sha256` |
| `edit_file` | Exact text replacement; unique match by default, optional `replace_all` and `expected_sha256` |
| `glob` / `grep` | Filename / content search; default 100 matches, at most 1000 and 12 KiB, 10-second timeout |
| `bash` | Noninteractive `/bin/bash -c`; returns a session ID if still running |
| `bash_poll` / `bash_list` / `bash_kill` | Continue output, find retained sessions, or kill a process group |

**Read and edit:** `offset` is a 1-based line number and `limit` is at most 2000. For truncated reads, continue with `next_byte_offset`, especially when `ends_mid_line=true`. Do not combine byte offsets with a line offset greater than 1. Line scans stop at 64 MiB; use a byte offset for more distant positions. Files up to 8 MiB also return a SHA256 for stale-content checks. Reads and edits reject special files, NUL bytes, and invalid UTF-8. Writes and edit text are limited to 64 KiB per call; editable files are limited to 8 MiB. Exact edits preserve CRLF; atomic replacement preserves permission bits, but does not provide transactional isolation from external writers.

**Search:** respects ignore rules, skips hidden files unless `hidden=true`, and does not recursively follow directory symlinks. File discovery stops at 10000 candidates or 1 MiB of paths. A truncated result means you should narrow the path or pattern; search has no stable pagination cursor.

**Bash:** default timeout 120 seconds, maximum 600. A tool call waits up to 10 seconds, then the command can continue in its session. Poll using `session_id` and `next_cursor` until both `running=false` and `has_more=false`. stdout/stderr are combined; each response is at most 12 KiB, with only the latest 256 KiB retained. `dropped_bytes` reports lost output; redirect large logs to a file. At most 8 sessions run at once and 32 are retained; completed sessions become eligible for cleanup after about 15 minutes. Sessions do not survive worker restarts. Timeout, termination, or worker shutdown kills the process group; processes that detach from it are outside that cleanup boundary. stdin is closed; interactive terminals are not supported.

## Running and maintaining the bridge

- **Stop:** press `Ctrl+C` in the worker terminal. This also terminates its managed Bash process groups.
- **Pause or revoke:** use the bridge dashboard. Pausing stops later claims and causes the worker to exit; resume and restart the worker to reconnect. Revocation invalidates Claude's grants. Neither action undoes work already executed.
- **Handle timeouts:** inspect the file or call `bash_list` before retrying. Jobs are claimed once and results can be retransmitted; operations are not automatically rerun. A timeout does not prove nothing happened.
- **Watch usage:** polling defaults to 2 seconds, about 43200 polls/day if run continuously, plus heartbeat and Redis traffic. `-poll 5s` reduces traffic with extra latency; keep it at 10 seconds or less. Free hosting plans have quotas and continuous operation is not guaranteed to be free.
- **Data retention:** job arguments and results are short-lived in Redis, normally deleted after a call and retained for about 90 seconds at most. The latest 20 metadata-only events expire within 24 hours. Access tokens last 1 hour; refresh tokens rotate on use and last up to 7 days, within an approximately 8-day grant lifetime.

Vercel Functions are configured for 60 seconds, with a default 45-second server wait per tool request. Long Bash execution stays on the worker, not in the Function. Run only one worker per deployment so session queries reach the computer holding those sessions.

## Local development

The credentials file generated earlier also supports a loopback-only development server with in-memory storage:

```sh
./bin/bridge-server -env .env.local
```

In a second terminal, from the repository directory:

```sh
./bin/bridge-worker -env .env.local -cwd /absolute/path/to/your/project -allow-http
```

Open `http://127.0.0.1:8787` for the dashboard. This is for local development and integration tests; Claude's cloud still needs the public HTTPS deployment. `-allow-http` permits loopback only. Production requires persistent Redis REST storage.

Run `make test` for race-enabled Go tests and vet checks. Coverage includes HTTP MCP → worker execution, shared server state, OAuth, stale grants, pagination, edits, special-file rejection, search bounds, and Bash lifecycle handling. Local tests do not replace client-side acceptance testing.

Cross-compile a worker, for example:

```sh
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o bin/bridge-worker-macos-arm64 ./cmd/worker
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/bridge-worker-linux-amd64 ./cmd/worker
```

Self-built macOS binaries are not Apple-notarized; distribution may require allowing the app through normal system prompts.

## Reference

- [Worker HTTP protocol v2 (Chinese)](docs/worker-protocol.md)
- [Environment variable template](.env.example)
- [Official Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk)
- [Claude remote custom connectors](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp)
- [Vercel Go runtime](https://vercel.com/docs/functions/runtimes/go)
