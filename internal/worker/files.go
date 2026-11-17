package worker

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/miownag/agent-local-bridge/internal/bridge"
)

type Files struct {
	root *os.Root
	Name string
}

func OpenFiles(dir string) (*Files, error) {
	absolute, e := filepath.Abs(dir)
	if e != nil {
		return nil, e
	}
	absolute, e = filepath.EvalSymlinks(absolute)
	if e != nil {
		return nil, errors.New("test directory must already exist")
	}
	if absolute == string(filepath.Separator) {
		return nil, errors.New("filesystem root cannot be used as a test directory")
	}
	home, _ := os.UserHomeDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	if absolute == home {
		return nil, errors.New("home directory cannot be used as a test directory")
	}
	r, e := os.OpenRoot(absolute)
	if e != nil {
		return nil, errors.New("cannot open test directory; create it before starting the worker")
	}
	return &Files{root: r, Name: filepath.Base(absolute)}, nil
}
func (f *Files) Close() error { return f.root.Close() }
func (f *Files) valid(path string, newFile bool) error {
	if len(path) > 1024 || !utf8.ValidString(path) || !filepath.IsLocal(path) || strings.ContainsAny(path, "\\\x00") {
		return errors.New("path must be relative to the test directory")
	}
	if path == "." {
		if newFile {
			return errors.New("a file path is required")
		}
		return nil
	}
	parts := strings.Split(path, "/")
	current := ""
	for i, part := range parts {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return errors.New("hidden paths and traversal are not allowed")
		}
		current = filepath.Join(current, part)
		s, e := f.root.Lstat(current)
		if e != nil {
			if newFile && i == len(parts)-1 && os.IsNotExist(e) {
				return nil
			}
			return errors.New("path does not exist or is inaccessible")
		}
		if s.Mode()&os.ModeSymlink != 0 {
			return errors.New("symbolic links are not allowed")
		}
		if i < len(parts)-1 && !s.IsDir() {
			return errors.New("parent path is not a directory")
		}
	}
	return nil
}
func (f *Files) Execute(job bridge.Job) bridge.Result {
	r := bridge.Result{ID: job.ID, Receipt: job.Receipt}
	if job.Deadline <= time.Now().Unix() {
		r.Error = "job expired before execution"
		return r
	}
	var out any
	var e error
	switch job.Tool {
	case "list_files":
		out, e = f.list(job.Arguments.Path)
	case "read_file":
		out, e = f.read(job.Arguments.Path)
	case "write_file":
		out, e = f.write(job.Arguments.Path, job.Arguments.Content)
	default:
		e = errors.New("unsupported tool")
	}
	if e != nil {
		r.Error = e.Error()
	} else {
		r.Output, _ = json.Marshal(out)
		if len(r.Output) > 96*1024 {
			r.Output = nil
			r.Error = "encoded result exceeds the 96 KiB response limit"
		}
	}
	return r
}

type Entry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func (f *Files) list(path string) (any, error) {
	if e := f.valid(path, false); e != nil {
		return nil, e
	}
	d, e := f.root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, errors.New("cannot open directory")
	}
	defer d.Close()
	s, e := d.Stat()
	if e != nil || !s.IsDir() {
		return nil, errors.New("path is not a directory")
	}
	entries, e := d.Readdir(201)
	if e != nil && e != io.EOF {
		return nil, errors.New("cannot list directory")
	}
	truncated := len(entries) > 200
	if truncated {
		entries = entries[:200]
	}
	out := []Entry{}
	for _, v := range entries {
		if strings.HasPrefix(v.Name(), ".") {
			continue
		}
		kind := "unsupported"
		if v.Mode().IsRegular() {
			kind = "file"
		} else if v.IsDir() {
			kind = "directory"
		} else if v.Mode()&os.ModeSymlink != 0 {
			kind = "symlink_blocked"
		}
		out = append(out, Entry{v.Name(), kind})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return map[string]any{"entries": out, "truncated": truncated}, nil
}
func (f *Files) read(path string) (any, error) {
	if e := f.valid(path, false); e != nil {
		return nil, e
	}
	d, e := f.root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, errors.New("cannot read file")
	}
	defer d.Close()
	s, e := d.Stat()
	if e != nil || !s.Mode().IsRegular() {
		return nil, errors.New("only regular files can be read")
	}
	b, e := io.ReadAll(io.LimitReader(d, bridge.MaxFileBytes+1))
	if e != nil {
		return nil, errors.New("cannot read file")
	}
	if len(b) > bridge.MaxFileBytes {
		return nil, errors.New("file exceeds 64 KiB limit")
	}
	if !utf8.Valid(b) {
		return nil, errors.New("file must contain UTF-8 text")
	}
	return map[string]any{"path": path, "content": string(b)}, nil
}
func (f *Files) write(path, content string) (any, error) {
	if len(content) > bridge.MaxFileBytes || !utf8.ValidString(content) {
		return nil, errors.New("content must be UTF-8 text of at most 64 KiB")
	}
	if e := f.valid(path, true); e != nil {
		return nil, e
	}
	d, e := f.root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return nil, errors.New("cannot create file; existing files cannot be overwritten")
	}
	_, e = d.WriteString(content)
	if e == nil {
		e = d.Sync()
	}
	closeErr := d.Close()
	if e != nil || closeErr != nil {
		return nil, errors.New("write failed; inspect the file before retrying")
	}
	return map[string]any{"path": path, "bytes": len(content), "created": true}, nil
}
