package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/miownag/agent-local-bridge/internal/bridge"
)

const outputBytes = 12 * 1024
const editBytes = 8 * 1024 * 1024

type Files struct {
	Name     string
	cwd      string
	mu       sync.Mutex
	sessions map[string]*shellSession
	closed   bool
}

func OpenFiles(dir string) (*Files, error) {
	if dir == "" {
		dir = "."
	}
	p, e := filepath.Abs(dir)
	if e != nil {
		return nil, e
	}
	s, e := os.Stat(p)
	if e != nil || !s.IsDir() {
		return nil, errors.New("working directory must exist")
	}
	name := filepath.Base(p)
	if name == string(filepath.Separator) {
		name = "filesystem"
	}
	return &Files{Name: name, cwd: p, sessions: make(map[string]*shellSession)}, nil
}
func (f *Files) Close() error {
	f.mu.Lock()
	f.closed = true
	sessions := make([]*shellSession, 0, len(f.sessions))
	for _, s := range f.sessions {
		sessions = append(sessions, s)
	}
	f.mu.Unlock()
	for _, s := range sessions {
		s.stop()
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for _, s := range sessions {
		select {
		case <-s.done:
		case <-timer.C:
			return errors.New("bash process cleanup timed out")
		}
	}
	return nil
}
func (f *Files) resolve(p string) (string, error) {
	if len(p) > 4096 || !utf8.ValidString(p) || strings.ContainsRune(p, 0) {
		return "", errors.New("invalid path")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		if p == "~" {
			p = h
		} else {
			p = filepath.Join(h, p[2:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(f.cwd, p)
	}
	return filepath.Clean(p), nil
}
func clip(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
func (f *Files) Execute(job bridge.Job) bridge.Result {
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(job.Deadline, 0))
	defer cancel()
	return f.ExecuteContext(ctx, job)
}
func (f *Files) ExecuteContext(ctx context.Context, job bridge.Job) bridge.Result {
	r := bridge.Result{ID: job.ID, Receipt: job.Receipt}
	if ctx.Err() != nil || job.Deadline <= time.Now().Unix() {
		r.Error = "job expired before execution"
		return r
	}
	var out any
	var e error
	a := job.Arguments
	switch job.Tool {
	case "list_files":
		out, e = f.list(a)
	case "read_file":
		out, e = f.read(ctx, a)
	case "write_file":
		out, e = f.write(a)
	case "edit_file":
		out, e = f.edit(a)
	case "glob", "grep":
		out, e = f.search(ctx, job.Tool, a)
	case "bash":
		out, e = f.bash(a)
	case "bash_list":
		out, e = f.listShells()
	case "bash_poll":
		out, e = f.pollShell(a, false)
	case "bash_kill":
		out, e = f.pollShell(a, true)
	default:
		e = errors.New("unsupported tool")
	}
	if e != nil {
		r.Error = clip(e.Error(), 160)
	} else {
		r.Output, _ = json.Marshal(out)
		if len(r.Output) > 96*1024 {
			r.Output = nil
			r.Error = "encoded result exceeds response limit"
		}
	}
	return r
}
func openRegular(p string) (*os.File, os.FileInfo, error) {
	d, e := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, nil, e
	}
	s, e := d.Stat()
	if e != nil || !s.Mode().IsRegular() {
		d.Close()
		return nil, nil, errors.New("only regular files are supported")
	}
	return d, s, nil
}
func digest(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func (f *Files) read(ctx context.Context, a bridge.Arguments) (any, error) {
	if a.Offset < 0 || a.Limit < 0 || a.Limit > 2000 || a.ByteOffset < 0 || (a.ByteOffset > 0 && a.Offset > 1) {
		return nil, errors.New("invalid range: use offset (1-based) or byte_offset, limit 1..2000")
	}
	p, e := f.resolve(a.Path)
	if e != nil {
		return nil, e
	}
	d, s, e := openRegular(p)
	if e != nil {
		return nil, e
	}
	defer d.Close()
	if a.ByteOffset > s.Size() {
		return nil, errors.New("byte_offset exceeds file size")
	}
	start := a.ByteOffset
	line := max(a.Offset, 1)
	limit := a.Limit
	if limit == 0 {
		limit = 200
	}
	if start > 0 {
		_, e = d.Seek(start, io.SeekStart)
		if e != nil {
			return nil, e
		}
	} else if line > 1 {
		br := bufio.NewReaderSize(d, 64*1024)
		for n := 1; n < line; {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			b, err := br.ReadSlice('\n')
			start += int64(len(b))
			if start > 64*1024*1024 {
				return nil, errors.New("line scan exceeds 64 MiB; use byte_offset")
			}
			if len(b) > 0 && b[len(b)-1] == '\n' {
				n++
			}
			if err != nil && err != bufio.ErrBufferFull {
				if err == io.EOF {
					break
				}
				return nil, err
			}
		}
		_, e = d.Seek(start, io.SeekStart)
		if e != nil {
			return nil, e
		}
	}
	b, e := io.ReadAll(io.LimitReader(d, outputBytes+4))
	if e != nil {
		return nil, e
	}
	end := min(len(b), outputBytes)
	count := 0
	for i := 0; i < end; i++ {
		if b[i] == '\n' {
			count++
			if count == limit {
				end = i + 1
				break
			}
		}
	}
	if len(b) > end {
		floor := max(0, end-3)
		for end > floor && !utf8.Valid(b[:end]) {
			end--
		}
	}
	b = b[:end]
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return nil, errors.New("binary or invalid UTF-8 file; use bash for binary inspection")
	}
	out := map[string]any{"path": p, "content": string(b), "size_bytes": s.Size(), "byte_offset": start, "next_byte_offset": start + int64(end), "truncated": start+int64(end) < s.Size()}
	if a.ByteOffset == 0 {
		out["start_line"] = line
		out["next_line"] = line + bytes.Count(b, []byte{'\n'})
		out["ends_mid_line"] = end > 0 && b[end-1] != '\n' && start+int64(end) < s.Size()
	}
	// Hash only editable files, streaming into a fixed-size buffer.
	if s.Size() <= editBytes {
		if _, e = d.Seek(0, io.SeekStart); e != nil {
			return nil, e
		}
		h := sha256.New()
		if _, e = io.Copy(h, io.LimitReader(d, editBytes+1)); e != nil {
			return nil, e
		}
		out["sha256"] = hex.EncodeToString(h.Sum(nil))
	}
	current, err := d.Stat()
	if err != nil {
		return nil, err
	}
	if current.Size() != s.Size() || !current.ModTime().Equal(s.ModTime()) {
		return nil, errors.New("file changed while reading; retry the read")
	}
	return out, nil
}
func (f *Files) list(a bridge.Arguments) (any, error) {
	if a.Offset < 0 || a.Limit < 0 || a.Limit > 1000 {
		return nil, errors.New("offset must be nonnegative, limit at most 1000")
	}
	p, e := f.resolve(a.Path)
	if e != nil {
		return nil, e
	}
	d, e := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	defer d.Close()
	s, e := d.Stat()
	if e != nil || !s.IsDir() {
		return nil, errors.New("path is not a directory")
	}
	limit := a.Limit
	if limit == 0 {
		limit = 200
	}
	// Directory offsets follow filesystem order to avoid loading enormous directories.
	skip := a.Offset
	for skip > 0 {
		batch, e := d.ReadDir(min(skip, 256))
		skip -= len(batch)
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
	}
	entries, e := d.ReadDir(limit + 1)
	if e != nil && e != io.EOF {
		return nil, e
	}
	more := len(entries) > limit
	if more {
		entries = entries[:limit]
	}
	out := []map[string]string{}
	used := 0
	for _, v := range entries {
		if used+len(v.Name())+64 > outputBytes {
			more = true
			break
		}
		kind := "file"
		if v.IsDir() {
			kind = "directory"
		} else if v.Type()&os.ModeSymlink != 0 {
			kind = "symlink"
		} else if v.Type() != 0 {
			kind = "special"
		}
		out = append(out, map[string]string{"name": v.Name(), "type": kind})
		encoded, _ := json.Marshal(out[len(out)-1])
		used += len(encoded)
	}
	return map[string]any{"path": p, "entries": out, "next_offset": a.Offset + len(out), "truncated": more}, nil
}
func editable(p string) ([]byte, os.FileInfo, error) {
	d, s, e := openRegular(p)
	if e != nil {
		return nil, nil, e
	}
	defer d.Close()
	if s.Size() > editBytes {
		return nil, nil, errors.New("editing limited to 8 MiB files; use bash for larger files")
	}
	b, e := io.ReadAll(io.LimitReader(d, editBytes+1))
	if e != nil {
		return nil, nil, e
	}
	if len(b) > editBytes || !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return nil, nil, errors.New("edit requires UTF-8 text up to 8 MiB")
	}
	return b, s, nil
}
func (f *Files) write(a bridge.Arguments) (any, error) {
	if len(a.Content) > bridge.MaxFileBytes || !utf8.ValidString(a.Content) || strings.ContainsRune(a.Content, 0) {
		return nil, errors.New("content must be UTF-8 text up to 64 KiB")
	}
	p, e := f.resolve(a.Path)
	if e != nil {
		return nil, e
	}
	if a.CreateParents {
		if e = os.MkdirAll(filepath.Dir(p), 0755); e != nil {
			return nil, e
		}
	}
	if !a.Overwrite {
		d, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, e
		}
		_, e = d.WriteString(a.Content)
		if e == nil {
			e = d.Sync()
		}
		ce := d.Close()
		if e != nil {
			return nil, e
		}
		if ce != nil {
			return nil, ce
		}
	} else {
		if a.ExpectedSHA256 == "" {
			return nil, errors.New("overwrite requires expected_sha256 from read_file")
		}
		old, s, e := editable(p)
		if e != nil {
			return nil, e
		}
		if digest(old) != a.ExpectedSHA256 {
			return nil, errors.New("file changed; read it again before writing")
		}
		if e = atomicReplace(p, old, []byte(a.Content), s.Mode()); e != nil {
			return nil, e
		}
	}
	return map[string]any{"path": p, "bytes": len(a.Content), "sha256": digest([]byte(a.Content)), "created": !a.Overwrite}, nil
}
func atomicReplace(p string, old, next []byte, mode os.FileMode) error {
	// Resolve existing links so atomic replacement updates the target, not the symlink.
	target, e := filepath.EvalSymlinks(p)
	if e != nil {
		return e
	}
	d, e := os.CreateTemp(filepath.Dir(target), ".bridge-edit-*")
	if e != nil {
		return e
	}
	defer os.Remove(d.Name())
	if e = d.Chmod(mode.Perm()); e == nil {
		_, e = d.Write(next)
	}
	if e == nil {
		e = d.Sync()
	}
	ce := d.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	now, _, e := editable(target)
	if e != nil {
		return e
	}
	if !bytes.Equal(now, old) {
		return errors.New("file changed during editing; read it again")
	}
	return os.Rename(d.Name(), target)
}
func (f *Files) edit(a bridge.Arguments) (any, error) {
	if a.OldText == "" {
		return nil, errors.New("old_text must be nonempty; use write_file for new files")
	}
	if len(a.OldText)+len(a.NewText) > bridge.MaxFileBytes || !utf8.ValidString(a.NewText) {
		return nil, errors.New("edit text exceeds 64 KiB or is not UTF-8")
	}
	p, e := f.resolve(a.Path)
	if e != nil {
		return nil, e
	}
	old, s, e := editable(p)
	if e != nil {
		return nil, e
	}
	if a.ExpectedSHA256 != "" && digest(old) != a.ExpectedSHA256 {
		return nil, errors.New("file changed; read it again before editing")
	}
	count := strings.Count(string(old), a.OldText)
	if count == 0 {
		return nil, errors.New("old_text not found; match whitespace and line endings exactly")
	}
	if count > 1 && !a.ReplaceAll {
		return nil, fmt.Errorf("old_text matches %d times; include more context or set replace_all", count)
	}
	if int64(len(old))+int64(count)*int64(len(a.NewText)-len(a.OldText)) > editBytes {
		return nil, errors.New("edited file would exceed 8 MiB")
	}
	next := []byte(strings.ReplaceAll(string(old), a.OldText, a.NewText))
	if bytes.IndexByte(next, 0) >= 0 {
		return nil, errors.New("edit requires text without NUL bytes")
	}
	if e = atomicReplace(p, old, next, s.Mode()); e != nil {
		return nil, e
	}
	idx := bytes.Index(old, []byte(a.OldText))
	line := bytes.Count(old[:idx], []byte{'\n'}) + 1
	return map[string]any{"path": p, "replacements": count, "first_changed_line": line, "sha256": digest(next), "bytes": len(next), "preview": clip("- "+a.OldText+"\n+ "+a.NewText, 4096), "preview_truncated": len(a.OldText)+len(a.NewText)+5 > 4096}, nil
}
