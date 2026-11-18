package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miownag/agent-local-bridge/internal/bridge"
)

func shellDone(t *testing.T, f *Files, v map[string]any) map[string]any {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for v["running"] == true && time.Now().Before(deadline) {
		v = execute(t, f, "bash_poll", bridge.Arguments{SessionID: v["session_id"].(string), Cursor: int64(v["next_cursor"].(float64)), YieldMS: 100})
	}
	if v["running"] == true {
		t.Fatal("shell never finished")
	}
	return v
}
func TestBashSessions(t *testing.T) {
	f := filesForTest(t)
	v := execute(t, f, "bash", bridge.Arguments{Command: "printf 'hello'; exit 7", YieldMS: 100})
	v = shellDone(t, f, v)
	if v["exit_code"] != float64(7) {
		t.Fatal(v)
	}
	v = execute(t, f, "bash", bridge.Arguments{Command: "sleep 0.2; printf 'finished'", YieldMS: 1})
	if v["running"] != true {
		t.Fatal("command blocked tool")
	}
	v = shellDone(t, f, v)
	if v["output"] != "finished" {
		t.Fatal(v)
	}
	v = execute(t, f, "bash", bridge.Arguments{Command: "(sleep 2; touch orphan) & wait", TimeoutSeconds: 1, YieldMS: 1})
	v = shellDone(t, f, v)
	if v["timed_out"] != true {
		t.Fatal(v)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, e := os.Stat(filepath.Join(f.cwd, "orphan")); !os.IsNotExist(e) {
		t.Fatal("timeout left a running child")
	}
	v = execute(t, f, "bash", bridge.Arguments{Command: "sleep 20", YieldMS: 1})
	v = execute(t, f, "bash_kill", bridge.Arguments{SessionID: v["session_id"].(string)})
	v = shellDone(t, f, v)
	if v["killed"] != true {
		t.Fatal(v)
	}
	fails(t, f, "bash_poll", bridge.Arguments{SessionID: "missing"})
}
func TestBashOutputBudgetAndShutdown(t *testing.T) {
	f := filesForTest(t)
	v := execute(t, f, "bash", bridge.Arguments{Command: "head -c 400000 /dev/zero | tr '\\0' x", YieldMS: 1000})
	v = shellDone(t, f, v)
	if v["dropped_bytes"].(float64) == 0 || len(v["output"].(string)) > outputBytes {
		t.Fatal(v)
	}
	v = execute(t, f, "bash", bridge.Arguments{Command: "sleep 30 & echo $! > child; wait", YieldMS: 100})
	id := v["session_id"].(string)
	f.Close()
	v = shellDone(t, f, v)
	child, e := os.ReadFile(filepath.Join(f.cwd, "child"))
	if e != nil {
		t.Fatal(e)
	}
	var pid int
	fmt.Sscanf(strings.TrimSpace(string(child)), "%d", &pid)
	if pid == 0 {
		t.Fatal("missing child pid")
	}
	// Killing the session after completion must not target an unrelated reused PID.
	v = execute(t, f, "bash_kill", bridge.Arguments{SessionID: id})
	if v["running"] != false {
		t.Fatal(v)
	}
	fails(t, f, "bash", bridge.Arguments{Command: "true"})
}
