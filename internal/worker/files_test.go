package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/miownag/agent-local-bridge/internal/bridge"
)

func filesForTest(t *testing.T) *Files {
	t.Helper()
	f, e := OpenFiles(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
func execute(t *testing.T, f *Files, tool string, a bridge.Arguments) map[string]any {
	t.Helper()
	r := f.Execute(bridge.Job{Tool: tool, Arguments: a, Deadline: time.Now().Add(time.Minute).Unix()})
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	var v map[string]any
	if e := json.Unmarshal(r.Output, &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func fails(t *testing.T, f *Files, tool string, a bridge.Arguments) {
	t.Helper()
	r := f.Execute(bridge.Job{Tool: tool, Arguments: a, Deadline: time.Now().Add(time.Minute).Unix()})
	if r.Error == "" {
		t.Fatalf("expected error: %s %+v", tool, a)
	}
}
func TestPathsAndSpecialFiles(t *testing.T) {
	f := filesForTest(t)
	outside := filepath.Join(t.TempDir(), ".hidden")
	os.WriteFile(outside, []byte("outside"), 0600)
	os.Symlink(outside, filepath.Join(f.cwd, "link"))
	for _, p := range []string{outside, "link", filepath.Join("..", filepath.Base(f.cwd), "link")} {
		v := execute(t, f, "read_file", bridge.Arguments{Path: p})
		if v["content"] != "outside" {
			t.Fatal(v)
		}
	}
	os.WriteFile(filepath.Join(f.cwd, "binary"), []byte{255, 0}, 0600)
	if e := syscall.Mkfifo(filepath.Join(f.cwd, "fifo"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{"binary", "fifo", "."} {
		fails(t, f, "read_file", bridge.Arguments{Path: p})
	}
	h, _ := os.UserHomeDir()
	p, e := f.resolve("~")
	if e != nil || p != h {
		t.Fatalf("home %q %v", p, e)
	}
	root, e := OpenFiles("/")
	if e != nil {
		t.Fatal(e)
	}
	root.Close()
}
func TestReadPaginationAndLongLines(t *testing.T) {
	f := filesForTest(t)
	text := strings.Repeat("你好世界\n", 4000)
	os.WriteFile(filepath.Join(f.cwd, "large"), []byte(text), 0600)
	v := execute(t, f, "read_file", bridge.Arguments{Path: "large", Offset: 2001, Limit: 2})
	if v["content"] != strings.Repeat("你好世界\n", 2) || v["next_line"] != float64(2003) || v["truncated"] != true {
		t.Fatal(v)
	}
	long := strings.Repeat("你", 20000) + "\nlast\n"
	os.WriteFile(filepath.Join(f.cwd, "long"), []byte(long), 0600)
	var joined strings.Builder
	var cursor int64
	for n := 0; n < 20; n++ {
		v = execute(t, f, "read_file", bridge.Arguments{Path: "long", ByteOffset: cursor})
		joined.WriteString(v["content"].(string))
		next := int64(v["next_byte_offset"].(float64))
		if next <= cursor {
			t.Fatal("pagination stalled")
		}
		cursor = next
		if !v["truncated"].(bool) {
			break
		}
	}
	if joined.String() != long {
		t.Fatal("long-line pagination lost bytes")
	}
	fails(t, f, "read_file", bridge.Arguments{Path: "long", Offset: 2, ByteOffset: 5})
	os.WriteFile(filepath.Join(f.cwd, "escaped"), []byte(strings.Repeat("\x01", 100000)), 0600)
	r := f.Execute(bridge.Job{Tool: "read_file", Arguments: bridge.Arguments{Path: "escaped"}, Deadline: time.Now().Add(time.Minute).Unix()})
	if r.Error != "" || len(r.Output) > 96*1024 {
		t.Fatal("JSON escaping exceeded wire budget", r.Error)
	}
}
func TestEditAndWrite(t *testing.T) {
	f := filesForTest(t)
	p := filepath.Join(f.cwd, "code")
	os.WriteFile(p, []byte("same\r\nsame\r\n"), 0755)
	v := execute(t, f, "read_file", bridge.Arguments{Path: p})
	hash := v["sha256"].(string)
	fails(t, f, "edit_file", bridge.Arguments{Path: p, OldText: "same", NewText: "new"})
	fails(t, f, "edit_file", bridge.Arguments{Path: p, OldText: "same", NewText: "new", ReplaceAll: true, ExpectedSHA256: "stale"})
	v = execute(t, f, "edit_file", bridge.Arguments{Path: p, OldText: "same", NewText: "new", ReplaceAll: true, ExpectedSHA256: hash})
	if v["replacements"] != float64(2) {
		t.Fatal(v)
	}
	b, _ := os.ReadFile(p)
	s, _ := os.Stat(p)
	if string(b) != "new\r\nnew\r\n" || s.Mode().Perm() != 0755 {
		t.Fatal("mode or CRLF changed")
	}
	fails(t, f, "write_file", bridge.Arguments{Path: p, Content: "bad"})
	fails(t, f, "write_file", bridge.Arguments{Path: p, Content: "bad", Overwrite: true})
	execute(t, f, "write_file", bridge.Arguments{Path: p, Content: "ok", Overwrite: true, ExpectedSHA256: v["sha256"].(string)})
	execute(t, f, "write_file", bridge.Arguments{Path: "a/b/.config", Content: "new", CreateParents: true})
	os.Symlink(p, filepath.Join(f.cwd, "link"))
	execute(t, f, "edit_file", bridge.Arguments{Path: "link", OldText: "ok", NewText: "linked"})
	if s, e := os.Lstat(filepath.Join(f.cwd, "link")); e != nil || s.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}
	b, _ = os.ReadFile(p)
	if string(b) != "linked" {
		t.Fatal("symlink target not edited")
	}
}
func TestExpiredJobHasNoSideEffects(t *testing.T) {
	f := filesForTest(t)
	r := f.Execute(bridge.Job{Tool: "write_file", Arguments: bridge.Arguments{Path: "new", Content: "bad"}, Deadline: time.Now().Add(-time.Second).Unix()})
	if r.Error == "" {
		t.Fatal("expired job ran")
	}
	if _, e := os.Stat(filepath.Join(f.cwd, "new")); !os.IsNotExist(e) {
		t.Fatal("expired write")
	}
}
