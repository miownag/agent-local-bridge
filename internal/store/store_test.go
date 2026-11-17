package store

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redisclient "github.com/redis/go-redis/v9"
)

func contract(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	if e := s.Set(ctx, "consume", "value", time.Minute); e != nil {
		t.Fatal(e)
	}
	var count atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, e := s.Take(ctx, "consume")
			if e != nil {
				t.Error(e)
			}
			if v != "" {
				count.Add(1)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("Take consumed %d times", count.Load())
	}
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.Increment(ctx, "counter", time.Minute)
			if e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	v, e := s.Get(ctx, "counter")
	if e != nil || v != "12" {
		t.Fatalf("counter = %s, %v", v, e)
	}
	ok, e := s.SetNX(ctx, "slot", "first", time.Minute)
	if !ok || e != nil {
		t.Fatalf("SetNX %v %v", ok, e)
	}
	ok, e = s.SetNX(ctx, "slot", "second", time.Minute)
	if ok || e != nil {
		t.Fatalf("duplicate SetNX %v %v", ok, e)
	}
	if e = s.DeleteIf(ctx, "slot", "second"); e != nil {
		t.Fatal(e)
	}
	v, _ = s.Get(ctx, "slot")
	if v != "first" {
		t.Fatal("stale owner deleted live slot")
	}
	if e = s.DeleteIf(ctx, "slot", "first"); e != nil {
		t.Fatal(e)
	}
	v, _ = s.Get(ctx, "slot")
	if v != "" {
		t.Fatal("slot not released")
	}
}
func TestMemoryContract(t *testing.T) { contract(t, NewMemory()) }
func TestRedisRESTContract(t *testing.T) {
	m := miniredis.RunT(t)
	client := redisclient.NewClient(&redisclient.Options{Addr: m.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer redis-test-token" {
			w.WriteHeader(401)
			return
		}
		var args []any
		if json.NewDecoder(r.Body).Decode(&args) != nil {
			w.WriteHeader(400)
			return
		}
		value, e := client.Do(r.Context(), args...).Result()
		if e == redisclient.Nil {
			e = nil
			value = nil
		}
		out := map[string]any{"result": value}
		if e != nil {
			out["error"] = e.Error()
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer rest.Close()
	s, e := NewRedis(rest.URL, "redis-test-token", "test")
	if e != nil {
		t.Fatal(e)
	}
	s.client = rest.Client()
	contract(t, s)
	if e = s.Set(context.Background(), "expires", "x", time.Second); e != nil {
		t.Fatal(e)
	}
	m.FastForward(2 * time.Second)
	v, e := s.Get(context.Background(), "expires")
	if e != nil || v != "" {
		t.Fatal("TTL did not expire", v, e)
	}
	if !m.Exists("test:counter") {
		t.Fatal("Redis namespace missing")
	}
}
