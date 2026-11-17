package bridge

import "encoding/json"

const MaxFileBytes = 64 * 1024
const MaxBodyBytes = 128 * 1024
const JobTTLSeconds = 90

type Arguments struct {
	Path    string `json:"path" jsonschema:"Relative path inside the selected test directory"`
	Content string `json:"content,omitempty" jsonschema:"UTF-8 content for a new file; existing files cannot be overwritten"`
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
}

type Status struct {
	Online   bool   `json:"online"`
	RootName string `json:"root_name,omitempty"`
}

type Event struct {
	ID    string `json:"id"`
	Tool  string `json:"tool"`
	State string `json:"state"`
	At    int64  `json:"at"`
}
