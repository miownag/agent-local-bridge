package server

import (
	"context"
	"encoding/json"

	"github.com/miownag/agent-local-bridge/internal/bridge"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type readInput struct {
	Path       string `json:"path" jsonschema:"File path: absolute, ~/..., or relative to worker cwd"`
	Offset     int    `json:"offset,omitempty" jsonschema:"Starting line, 1-based; default 1. Do not combine with byte_offset"`
	Limit      int    `json:"limit,omitempty" jsonschema:"Maximum lines, default 200, at most 2000. Output also capped at 12 KiB"`
	ByteOffset int64  `json:"byte_offset,omitempty" jsonschema:"Resume at next_byte_offset for long lines or large files"`
}
type listInput struct {
	Path   string `json:"path,omitempty" jsonschema:"Directory; default worker cwd. Includes hidden files"`
	Offset int    `json:"offset,omitempty" jsonschema:"Zero-based entry offset, default 0; use returned next_offset"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Default 200, maximum 1000 entries"`
}
type writeInput struct {
	Path           string `json:"path"`
	Content        string `json:"content" jsonschema:"Complete UTF-8 file content, at most 64 KiB"`
	Overwrite      bool   `json:"overwrite,omitempty" jsonschema:"Default false. Existing files require overwrite and expected_sha256"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty" jsonschema:"SHA256 from read_file; mandatory when overwriting"`
	CreateParents  bool   `json:"create_parents,omitempty" jsonschema:"Create missing parent directories"`
}
type editInput struct {
	Path           string `json:"path"`
	OldText        string `json:"old_text" jsonschema:"Exact nonempty text to replace; preserve whitespace and CRLF line endings"`
	NewText        string `json:"new_text" jsonschema:"Replacement text; empty deletes old_text"`
	ReplaceAll     bool   `json:"replace_all,omitempty" jsonschema:"Default false; ambiguous matches otherwise fail"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty" jsonschema:"Use SHA256 from read_file to reject stale edits"`
}
type globInput struct {
	Pattern string `json:"pattern" jsonschema:"Ripgrep glob pattern, e.g. **/*.go"`
	Path    string `json:"path,omitempty" jsonschema:"Search directory; default worker cwd"`
	Limit   int    `json:"limit,omitempty" jsonschema:"Maximum matches, default 100, at most 1000"`
	Hidden  bool   `json:"hidden,omitempty" jsonschema:"Include dotfiles; .gitignore is still respected"`
}
type grepInput struct {
	Pattern    string `json:"pattern" jsonschema:"Ripgrep regex, or literal text with fixed=true"`
	Path       string `json:"path,omitempty" jsonschema:"Search file or directory; default worker cwd"`
	Glob       string `json:"glob,omitempty" jsonschema:"Restrict filenames, e.g. *.go"`
	Limit      int    `json:"limit,omitempty" jsonschema:"Maximum result lines, default 100, at most 1000"`
	IgnoreCase bool   `json:"ignore_case,omitempty"`
	Fixed      bool   `json:"fixed,omitempty"`
	Hidden     bool   `json:"hidden,omitempty"`
}
type bashInput struct {
	Command        string `json:"command" jsonschema:"Bash script, at most 32 KiB. Runs with worker user's full permissions"`
	Cwd            string `json:"cwd,omitempty" jsonschema:"Existing working directory, default worker cwd; does not persist across calls"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Wall timeout, default 120 seconds, maximum 600; kills process group"`
	YieldMS        int    `json:"yield_ms,omitempty" jsonschema:"Wait 1..10000 ms before returning; default 1000. If running, use bash_poll"`
}
type pollInput struct {
	SessionID string `json:"session_id"`
	Cursor    int64  `json:"cursor,omitempty" jsonschema:"Byte cursor from next_cursor; default 0"`
	YieldMS   int    `json:"yield_ms,omitempty" jsonschema:"Wait up to 10000 ms, default 1000"`
}
type killInput struct {
	SessionID string `json:"session_id"`
}

func addWorkerTool[I any](a *App, ms *mcp.Server, name, description string, readOnly bool) {
	open, destructive := true, !readOnly
	mcp.AddTool(ms, &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly, DestructiveHint: &destructive, OpenWorldHint: &open}}, func(ctx context.Context, _ *mcp.CallToolRequest, input I) (*mcp.CallToolResult, any, error) {
		b, e := json.Marshal(input)
		if e != nil {
			return nil, nil, e
		}
		var args bridge.Arguments
		if e = json.Unmarshal(b, &args); e != nil {
			return nil, nil, e
		}
		out, e := a.dispatch(ctx, name, args)
		return nil, out, e
	})
}
func (a *App) addWorkerTools(ms *mcp.Server) {
	addWorkerTool[listInput](a, ms, "list_files", "List a directory including hidden entries. Paginated in filesystem order; concurrent directory changes can shift offsets.", true)
	addWorkerTool[readInput](a, ms, "read_file", "Read UTF-8 text in bounded pages (200 lines / 12 KiB default), including large files. Follow next_byte_offset when truncated; next_line is valid only if ends_mid_line=false. Returns SHA256 for files up to 8 MiB. Binary files are rejected.", true)
	addWorkerTool[writeInput](a, ms, "write_file", "Create a UTF-8 file, optionally creating parents. To overwrite, read first and supply expected_sha256. Atomic replacement preserves permission bits; maximum content 64 KiB.", false)
	addWorkerTool[editInput](a, ms, "edit_file", "Precisely replace exact text in an existing UTF-8 file up to 8 MiB. Read first; use expected_sha256 for stale-write protection. Ambiguous matches fail unless replace_all=true. Returns a bounded change preview, not the entire file.", false)
	addWorkerTool[globInput](a, ms, "glob", "Find files using ripgrep globs. Respects gitignore; bounded to 12 KiB / limit matches, times out after 10 seconds. Narrow path/pattern if truncated. Requires rg on worker.", true)
	addWorkerTool[grepInput](a, ms, "grep", "Search file contents using ripgrep; returns filename:line:text, respects gitignore, skips binary files. Lines over 500 columns are abbreviated. Bounded output; narrow query if truncated. Requires rg on worker.", true)
	addWorkerTool[bashInput](a, ms, "bash", "Execute /bin/bash -c on the local computer. No directory sandbox; inherits worker environment and permissions. No stdin. Returns a session for running commands; poll rather than rerunning. Output is paginated and retains only the latest 256 KiB; redirect large logs to a file. Background descendants are killed when the shell exits. Sessions do not survive restarts.", false)
	addWorkerTool[struct{}](a, ms, "bash_list", "List retained bash sessions with command previews. Use after a lost response or timeout before deciding whether to retry a command.", true)
	addWorkerTool[pollInput](a, ms, "bash_poll", "Read bash output from cursor and wait briefly for completion. Continue while running or has_more. dropped_bytes reports discarded output; inspect redirected logs when needed. Up to 32 sessions retained, completed sessions expire after 15 minutes.", true)
	addWorkerTool[killInput](a, ms, "bash_kill", "Kill a bash session and its process group; already completed sessions are unchanged.", false)
}
