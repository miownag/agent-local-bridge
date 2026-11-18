package bridge

import "encoding/json"

const MaxFileBytes = 64 * 1024
const MaxBodyBytes = 128 * 1024
const JobTTLSeconds = 90
const ProtocolVersion = 2

// Arguments is the worker wire format. Each MCP tool has a narrower input schema.
type Arguments struct {
	Path           string `json:"path,omitempty"`
	Content        string `json:"content,omitempty"`
	Offset         int    `json:"offset,omitempty"`
	Limit          int    `json:"limit,omitempty"`
	ByteOffset     int64  `json:"byte_offset,omitempty"`
	OldText        string `json:"old_text,omitempty"`
	NewText        string `json:"new_text,omitempty"`
	ReplaceAll     bool   `json:"replace_all,omitempty"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	Overwrite      bool   `json:"overwrite,omitempty"`
	CreateParents  bool   `json:"create_parents,omitempty"`
	Pattern        string `json:"pattern,omitempty"`
	Glob           string `json:"glob,omitempty"`
	IgnoreCase     bool   `json:"ignore_case,omitempty"`
	Fixed          bool   `json:"fixed,omitempty"`
	Hidden         bool   `json:"hidden,omitempty"`
	Command        string `json:"command,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	YieldMS        int    `json:"yield_ms,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	Cursor         int64  `json:"cursor,omitempty"`
}

type Job struct {
	ID        string    `json:"id"`
	Tool      string    `json:"tool"`
	Arguments Arguments `json:"arguments"`
	Deadline  int64     `json:"deadline"`
	Receipt   string    `json:"receipt"`
}
type Result struct {
	ID      string          `json:"id"`
	Receipt string          `json:"receipt"`
	Output  json.RawMessage `json:"output,omitempty"`
	Error   string          `json:"error,omitempty"`
}
type Heartbeat struct {
	RootName string `json:"root_name"`
	At       int64  `json:"at"`
	Protocol int    `json:"protocol,omitempty"`
}
type Status struct {
	Online   bool   `json:"online"`
	RootName string `json:"root_name,omitempty"`
	Protocol int    `json:"protocol,omitempty"`
}
type Event struct {
	ID    string `json:"id"`
	Tool  string `json:"tool"`
	State string `json:"state"`
	At    int64  `json:"at"`
}
