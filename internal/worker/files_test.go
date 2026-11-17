package worker

import (
	"github.com/miownag/agent-local-bridge/internal/bridge"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFilesRejectUnsafeTargets(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret"), []byte("outside-data"), 0600)
	if e := os.Symlink(outside, filepath.Join(dir, "escape")); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink("normal", filepath.Join(dir, "inside-link")); e != nil {
		t.Fatal(e)
	}
	_ = os.WriteFile(filepath.Join(dir, "normal"), []byte("normal"), 0600)
	_ = os.WriteFile(filepath.Join(dir, "large"), []byte(strings.Repeat("x", bridge.MaxFileBytes+1)), 0600)
	_ = os.WriteFile(filepath.Join(dir, "binary"), []byte{255, 254}, 0600)
	if e := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0600); e != nil {
		t.Fatal(e)
	}
	f, e := OpenFiles(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	for _, path := range []string{"../secret", filepath.Join(outside, "secret"), "escape/secret", "inside-link", "large", "binary", "fifo", ".hidden", "normal/../normal"} {
		t.Run(path, func(t *testing.T) {
			r := f.Execute(bridge.Job{Tool: "read_file", Arguments: bridge.Arguments{Path: path}, Deadline: time.Now().Add(time.Minute).Unix()})
			if r.Error == "" {
				t.Fatalf("unsafe target accepted: %s", path)
			}
			if strings.Contains(string(r.Output), "outside-data") {
				t.Fatal("data escaped root")
			}
		})
	}
	for _, path := range []string{"normal", "escape/new", "../new", "/tmp/new", "inside-link"} {
		r := f.Execute(bridge.Job{Tool: "write_file", Arguments: bridge.Arguments{Path: path, Content: "bad"}, Deadline: time.Now().Add(time.Minute).Unix()})
		if r.Error == "" {
			t.Fatalf("unsafe write accepted: %s", path)
		}
	}
	data, _ := os.ReadFile(filepath.Join(dir, "normal"))
	if string(data) != "normal" {
		t.Fatal("existing file changed")
	}
}
func TestExpiredJobHasNoSideEffects(t *testing.T) {
	f, e := OpenFiles(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	r := f.Execute(bridge.Job{Tool: "write_file", Arguments: bridge.Arguments{Path: "new", Content: "test"}, Deadline: time.Now().Add(-time.Second).Unix()})
	if r.Error == "" {
		t.Fatal("expired job ran")
	}
	if _, e := f.root.Stat("new"); !os.IsNotExist(e) {
		t.Fatal("expired job created a file")
	}
}

func TestEncodedOutputLimit(t *testing.T) {
	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, "escaped"), []byte(strings.Repeat("\x01", bridge.MaxFileBytes)), 0600); e != nil {
		t.Fatal(e)
	}
	f, e := OpenFiles(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	r := f.Execute(bridge.Job{Tool: "read_file", Arguments: bridge.Arguments{Path: "escaped"}, Deadline: time.Now().Add(time.Minute).Unix()})
	if r.Error == "" || len(r.Output) != 0 {
		t.Fatal("oversized JSON result was accepted")
	}
}
