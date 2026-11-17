package handler

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// Vercel compiles api under a generated handler module rather than our module path.
func TestVercelGeneratedModuleCompiles(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.Mkdir(filepath.Join(dir, "api"), 0700); err != nil {
		t.Fatal(err)
	}
	entry, err := os.ReadFile("index.go")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"go.mod":       []byte("module handler\n\ngo 1.25.0\n\nrequire github.com/miownag/agent-local-bridge v0.0.0\nreplace github.com/miownag/agent-local-bridge => " + strconv.Quote(root) + "\n"),
		"api/index.go": entry,
		"main.go":      []byte("package main\nimport (\"handler/api\"; \"net/http\")\nfunc main(){http.HandleFunc(\"/\",handler.Handler)}\n"),
	}
	for name, data := range files {
		if err = os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "build", "-mod=mod", "-o", filepath.Join(dir, "bootstrap"), ".")
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Vercel-style build failed: %v\n%s", err, output)
	}
}
