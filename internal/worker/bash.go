package worker

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/miownag/agent-local-bridge/internal/bridge"
)

const shellBufferBytes = 256 * 1024

type shellSession struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	data      []byte
	total     int64
	done      chan struct{}
	exit      int
	timedOut  bool
	killed    bool
	finished  bool
	created   time.Time
	completed time.Time
	command   string
	cwd       string
}

func (s *shellSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	s.total += int64(n)
	if n >= shellBufferBytes {
		s.data = append(s.data[:0], p[n-shellBufferBytes:]...)
	} else {
		overflow := len(s.data) + n - shellBufferBytes
		if overflow > 0 {
			copy(s.data, s.data[overflow:])
			s.data = s.data[:len(s.data)-overflow]
		}
		s.data = append(s.data, p...)
	}
	return n, nil
}
func (s *shellSession) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.killed = true
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
}
func (f *Files) bash(a bridge.Arguments) (any, error) {
	if a.Command == "" || len(a.Command) > 32*1024 {
		return nil, errors.New("command must contain 1..32768 bytes")
	}
	timeout := a.TimeoutSeconds
	if timeout == 0 {
		timeout = 120
	}
	if timeout < 1 || timeout > 600 {
		return nil, errors.New("timeout_seconds must be 1..600")
	}
	if a.YieldMS < 0 || a.YieldMS > 10000 {
		return nil, errors.New("yield_ms must be 0..10000")
	}
	cwd, e := f.resolve(a.Cwd)
	if e != nil {
		return nil, e
	}
	st, e := os.Stat(cwd)
	if e != nil || !st.IsDir() {
		return nil, errors.New("cwd must be an existing directory")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, errors.New("worker is closing")
	}
	active := 0
	for id, s := range f.sessions {
		select {
		case <-s.done:
			if time.Since(s.completed) > 15*time.Minute {
				delete(f.sessions, id)
			}
		default:
			active++
		}
	}
	if active >= 8 {
		return nil, errors.New("8 bash sessions are running; poll or kill an existing session")
	}
	if len(f.sessions) >= 32 {
		var oldest string
		var at time.Time
		for id, s := range f.sessions {
			select {
			case <-s.done:
				if oldest == "" || s.created.Before(at) {
					oldest = id
					at = s.created
				}
			default:
			}
		}
		if oldest != "" {
			delete(f.sessions, oldest)
		}
	}
	var idBytes [16]byte
	if _, e = rand.Read(idBytes[:]); e != nil {
		return nil, e
	}
	id := hex.EncodeToString(idBytes[:])
	cmd := exec.Command("/bin/bash", "-c", a.Command)
	cmd.Dir = cwd
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	s := &shellSession{cmd: cmd, done: make(chan struct{}), created: time.Now(), cwd: cwd, command: clip(a.Command, 256), exit: -1}
	cmd.Stdout = s
	cmd.Stderr = s
	if e = cmd.Start(); e != nil {
		return nil, e
	}
	f.sessions[id] = s
	go func() {
		timer := time.NewTimer(time.Duration(timeout) * time.Second)
		defer timer.Stop()
		select {
		case <-s.done:
			return
		case <-timer.C:
			s.mu.Lock()
			if s.finished {
				s.mu.Unlock()
				return
			}
			s.timedOut = true
			s.mu.Unlock()
			s.stop()
		}
	}()
	go func() {
		e := cmd.Wait()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		s.mu.Lock()
		s.finished = true
		s.completed = time.Now()
		s.exit = cmd.ProcessState.ExitCode()
		if e == nil {
			s.exit = 0
		}
		s.mu.Unlock()
		close(s.done)
	}()
	// Release the registry lock while waiting so Close can terminate the process.
	f.mu.Unlock()
	waitShell(s, a.YieldMS)
	out := shellOutput(id, s, 0)
	f.mu.Lock()
	return out, nil
}
func waitShell(s *shellSession, ms int) {
	if ms == 0 {
		ms = 1000
	}
	timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-s.done:
	case <-timer.C:
	}
}
func shellOutput(id string, s *shellSession, cursor int64) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := s.total - int64(len(s.data))
	start := max(cursor, base)
	start = min(start, s.total)
	end := min(start+outputBytes, s.total)
	data := s.data[start-base : end-base]
	// Cursor positions count original bytes, including bytes replaced while rendering UTF-8.
	running := true
	select {
	case <-s.done:
		running = false
	default:
	}
	return map[string]any{"session_id": id, "cwd": s.cwd, "output": stringifyOutput(data), "cursor": start, "next_cursor": end, "total_bytes": s.total, "dropped_bytes": max(int64(0), base-cursor), "has_more": end < s.total, "running": running, "exit_code": s.exit, "timed_out": s.timedOut, "killed": s.killed}
}
func (f *Files) pollShell(a bridge.Arguments, kill bool) (any, error) {
	if a.Cursor < 0 || a.YieldMS < 0 || a.YieldMS > 10000 {
		return nil, errors.New("cursor must be nonnegative and yield_ms at most 10000")
	}
	f.mu.Lock()
	s := f.sessions[a.SessionID]
	f.mu.Unlock()
	if s == nil {
		return nil, errors.New("unknown or expired bash session; sessions do not survive worker restarts")
	}
	s.mu.Lock()
	invalid := a.Cursor > s.total
	s.mu.Unlock()
	if invalid {
		return nil, errors.New("cursor exceeds session output")
	}
	if kill {
		s.stop()
		waitShell(s, 1000)
	} else {
		waitShell(s, a.YieldMS)
	}
	return shellOutput(a.SessionID, s, a.Cursor), nil
}
func stringifyOutput(b []byte) string { return string([]rune(string(b))) }

func (f *Files) listShells() (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.sessions))
	for id := range f.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := []map[string]any{}
	for _, id := range ids {
		s := f.sessions[id]
		s.mu.Lock()
		out = append(out, map[string]any{"session_id": id, "command": s.command, "cwd": s.cwd, "running": !s.finished, "exit_code": s.exit, "total_bytes": s.total, "started_at": s.created.UTC().Format(time.RFC3339)})
		s.mu.Unlock()
	}
	return map[string]any{"sessions": out}, nil
}
