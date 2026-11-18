package worker

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/miownag/agent-local-bridge/internal/bridge"
)

type boundedOutput struct {
	mu    sync.Mutex
	data  []byte
	total int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += len(p)
	if len(b.data) < outputBytes {
		b.data = append(b.data, p[:min(len(p), outputBytes-len(b.data))]...)
	}
	return len(p), nil
}
func rgError(e error, stderr *boundedOutput) error {
	if e == nil {
		return nil
	}
	var exit *exec.ExitError
	if errors.As(e, &exit) && exit.ExitCode() == 1 {
		return nil
	}
	return errors.New("ripgrep failed: " + clip(string(stderr.data), 100))
}
func (f *Files) search(ctx context.Context, tool string, a bridge.Arguments) (any, error) {
	if a.Pattern == "" || len(a.Pattern) > 4096 || len(a.Glob) > 4096 {
		return nil, errors.New("pattern required, at most 4096 bytes")
	}
	if a.Limit < 0 || a.Limit > 1000 {
		return nil, errors.New("limit must be 1..1000")
	}
	limit := a.Limit
	if limit == 0 {
		limit = 100
	}
	filter := a.Glob
	if tool == "glob" {
		filter = a.Pattern
	}
	if filter != "" && !doublestar.ValidatePattern(filter) {
		return nil, errors.New("invalid glob pattern")
	}
	rg, e := exec.LookPath("rg")
	if e != nil {
		return nil, errors.New("glob/grep require ripgrep (rg); install with brew install ripgrep")
	}
	p, e := f.resolve(a.Path)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Enumerate without --glob: positive rg globs override gitignore and hidden-file rules.
	filesCtx, stopFiles := context.WithCancel(ctx)
	defer stopFiles()
	args := []string{"--files", "--null"}
	if a.Hidden {
		args = append(args, "--hidden")
	}
	args = append(args, "--", p)
	cmd := exec.CommandContext(filesCtx, rg, args...)
	cmd.Dir = f.cwd
	cmd.WaitDelay = time.Second
	stderr := &boundedOutput{}
	cmd.Stderr = stderr
	pipe, e := cmd.StdoutPipe()
	if e != nil {
		return nil, e
	}
	if e = cmd.Start(); e != nil {
		return nil, e
	}
	reader := bufio.NewReader(pipe)
	files := []string{}
	bytesUsed := 0
	truncated := false
	for {
		name, err := reader.ReadString(0)
		if err == nil {
			name = strings.TrimSuffix(name, "\x00")
			relative, _ := filepath.Rel(p, name)
			if relative == "." {
				relative = filepath.Base(name)
			}
			candidate := filepath.ToSlash(relative)
			if !strings.Contains(filter, "/") {
				candidate = filepath.Base(name)
			}
			matched := filter == ""
			if !matched {
				matched, _ = doublestar.Match(filter, candidate)
			}
			if matched {
				files = append(files, name)
				bytesUsed += len(name)
			}
			cap := 10000
			if tool == "glob" {
				cap = limit + 1
			}
			if len(files) >= cap || bytesUsed > 1024*1024 {
				truncated = true
				stopFiles()
				break
			}
		} else if err == io.EOF {
			break
		} else {
			stopFiles()
			cmd.Wait()
			return nil, err
		}
	}
	e = cmd.Wait()
	if ctx.Err() != nil {
		return nil, errors.New("search timed out; narrow path or pattern")
	}
	if !truncated {
		if e = rgError(e, stderr); e != nil {
			return nil, e
		}
	}
	matches := []string{}
	used := 0
	appendMatch := func(line string) bool {
		if len(matches) >= limit || used+len(line) > outputBytes {
			truncated = true
			return false
		}
		matches = append(matches, line)
		used += len(line)
		return true
	}
	if tool == "glob" {
		for _, name := range files {
			if !appendMatch(name) {
				break
			}
		}
	} else {
		for start := 0; start < len(files); {
			end := start
			argBytes := 0
			for end < len(files) && argBytes+len(files[end]) < 48*1024 {
				argBytes += len(files[end])
				end++
			}
			if end == start {
				return nil, errors.New("file path exceeds argument limit")
			}
			args = []string{"--color=never", "--line-number", "--no-heading", "--with-filename", "--max-columns=500", "--max-columns-preview", "--max-count", "1000"}
			if a.Fixed {
				args = append(args, "--fixed-strings")
			}
			if a.IgnoreCase {
				args = append(args, "--ignore-case")
			}
			args = append(args, "--regexp", a.Pattern, "--")
			args = append(args, files[start:end]...)
			cmd = exec.CommandContext(ctx, rg, args...)
			cmd.Dir = f.cwd
			cmd.WaitDelay = time.Second
			output, errout := &boundedOutput{}, &boundedOutput{}
			cmd.Stdout = output
			cmd.Stderr = errout
			e = cmd.Run()
			if ctx.Err() != nil {
				return nil, errors.New("search timed out; narrow path or pattern")
			}
			if e = rgError(e, errout); e != nil {
				return nil, e
			}
			text := string(output.data)
			if output.total > len(output.data) {
				truncated = true
				if n := strings.LastIndexByte(text, '\n'); n >= 0 {
					text = text[:n+1]
				} else {
					text = ""
				}
			}
			full := false
			for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
				if line != "" && !appendMatch(line) {
					full = true
					break
				}
			}
			if full || output.total > len(output.data) {
				break
			}
			start = end
		}
	}
	return map[string]any{"path": p, "matches": matches, "count": len(matches), "truncated": truncated, "hint": "Respects gitignore; set hidden for dotfiles. Discovery capped at 10000 files / 1 MiB paths; narrow path/glob if truncated."}, nil
}
