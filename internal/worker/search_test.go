package worker

import (
	"github.com/miownag/agent-local-bridge/internal/bridge"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSearchIgnoresAndBounds(t *testing.T) {
	if _, e := exec.LookPath("rg"); e != nil {
		t.Skip("rg not installed")
	}
	f := filesForTest(t)
	os.Mkdir(filepath.Join(f.cwd, ".git"), 0755)
	os.WriteFile(filepath.Join(f.cwd, ".gitignore"), []byte("ignored.go\n"), 0600)
	for _, name := range []string{"a.go", "b.go", "ignored.go", ".hidden.go"} {
		os.WriteFile(filepath.Join(f.cwd, name), []byte("needle\n"), 0600)
	}
	v := execute(t, f, "glob", bridge.Arguments{Pattern: "*.go"})
	if v["count"] != float64(2) {
		t.Fatal(v)
	}
	v = execute(t, f, "grep", bridge.Arguments{Pattern: "needle", Limit: 1})
	if v["count"] != float64(1) || v["truncated"] != true {
		t.Fatal(v)
	}
	v = execute(t, f, "grep", bridge.Arguments{Pattern: "needle", Hidden: true, Glob: "*.go"})
	if v["count"] != float64(3) {
		t.Fatal(v)
	}
	fails(t, f, "grep", bridge.Arguments{Pattern: "["})
}
