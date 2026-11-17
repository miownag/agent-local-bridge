package bridge

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadEnv reads literal KEY=VALUE lines without evaluating shell expressions.
func LoadEnv(path string) error {
	if path == "" {
		return nil
	}
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	line := 0
	for s.Scan() {
		line++
		text := strings.TrimSpace(s.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		k, v, ok := strings.Cut(text, "=")
		if !ok || k == "" || strings.TrimSpace(k) != k {
			return fmt.Errorf("invalid environment line %d", line)
		}
		if !strings.HasPrefix(k, "BRIDGE_") && !strings.HasPrefix(k, "UPSTASH_REDIS_REST_") {
			return fmt.Errorf("unsupported environment key on line %d", line)
		}
		if _, exists := os.LookupEnv(k); !exists {
			if e := os.Setenv(k, v); e != nil {
				return e
			}
		}
	}
	return s.Err()
}
