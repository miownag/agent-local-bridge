package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Store operations are atomic across all server instances.
type Store interface {
	Get(context.Context, string) (string, error)
	Set(context.Context, string, string, time.Duration) error
	SetNX(context.Context, string, string, time.Duration) (bool, error)
	Take(context.Context, string) (string, error)
	DeleteIf(context.Context, string, string) error
	Increment(context.Context, string, time.Duration) (int, error)
}

type item struct {
	value   string
	expires time.Time
}
type Memory struct {
	mu   sync.Mutex
	data map[string]item
}

// NewMemory is for local development and tests only.
func NewMemory() *Memory { return &Memory{data: make(map[string]item)} }
func (m *Memory) get(k string) string {
	i, ok := m.data[k]
	if !ok || time.Now().After(i.expires) {
		delete(m.data, k)
		return ""
	}
	return i.value
}
func (m *Memory) Get(_ context.Context, k string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.get(k), nil
}
func (m *Memory) Set(_ context.Context, k, v string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[k] = item{v, time.Now().Add(ttl)}
	return nil
}
func (m *Memory) SetNX(_ context.Context, k, v string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.get(k) != "" {
		return false, nil
	}
	m.data[k] = item{v, time.Now().Add(ttl)}
	return true, nil
}
func (m *Memory) Take(_ context.Context, k string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.get(k)
	delete(m.data, k)
	return v, nil
}
func (m *Memory) DeleteIf(_ context.Context, k, v string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.get(k) == v {
		delete(m.data, k)
	}
	return nil
}
func (m *Memory) Increment(_ context.Context, k string, ttl time.Duration) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.get(k)
	n, _ := strconv.Atoi(v)
	i := m.data[k]
	if v == "" {
		i.expires = time.Now().Add(ttl)
	}
	n++
	i.value = strconv.Itoa(n)
	m.data[k] = i
	return n, nil
}

type Redis struct {
	endpoint, token, prefix string
	client                  *http.Client
}

func NewRedis(endpoint, token, prefix string) (*Redis, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("Redis REST URL must be HTTPS without credentials, query or fragment")
	}
	if token == "" || prefix == "" {
		return nil, errors.New("Redis token and namespace are required")
	}
	return &Redis{strings.TrimRight(endpoint, "/"), token, prefix, &http.Client{Timeout: 10 * time.Second}}, nil
}
func (r *Redis) key(k string) string { return r.prefix + ":" + k }
func (r *Redis) command(ctx context.Context, args ...any) (json.RawMessage, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", r.endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := r.client.Do(req)
	if err != nil {
		return nil, errors.New("Redis request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("Redis returned HTTP %d", res.StatusCode)
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 1024*1024)).Decode(&out); err != nil {
		return nil, errors.New("invalid Redis response")
	}
	if out.Error != "" {
		return nil, errors.New("Redis command failed")
	}
	return out.Result, nil
}
func (r *Redis) Get(ctx context.Context, k string) (string, error) {
	v, e := r.command(ctx, "GET", r.key(k))
	if e != nil {
		return "", e
	}
	var s string
	if string(v) == "null" {
		return "", nil
	}
	e = json.Unmarshal(v, &s)
	return s, e
}
func (r *Redis) Set(ctx context.Context, k, v string, ttl time.Duration) error {
	_, e := r.command(ctx, "SET", r.key(k), v, "PX", ttl.Milliseconds())
	return e
}
func (r *Redis) SetNX(ctx context.Context, k, v string, ttl time.Duration) (bool, error) {
	out, e := r.command(ctx, "SET", r.key(k), v, "PX", ttl.Milliseconds(), "NX")
	return string(out) == `"OK"`, e
}
func (r *Redis) Take(ctx context.Context, k string) (string, error) {
	v, e := r.command(ctx, "GETDEL", r.key(k))
	if e != nil {
		return "", e
	}
	if string(v) == "null" {
		return "", nil
	}
	var s string
	e = json.Unmarshal(v, &s)
	return s, e
}
func (r *Redis) DeleteIf(ctx context.Context, k, v string) error {
	_, e := r.command(ctx, "EVAL", `if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('DEL',KEYS[1]) end return 0`, 1, r.key(k), v)
	return e
}
func (r *Redis) Increment(ctx context.Context, k string, ttl time.Duration) (int, error) {
	v, e := r.command(ctx, "EVAL", `local n=redis.call('INCR',KEYS[1]); if n==1 then redis.call('PEXPIRE',KEYS[1],ARGV[1]) end return n`, 1, r.key(k), ttl.Milliseconds())
	if e != nil {
		return 0, e
	}
	var n int
	e = json.Unmarshal(v, &n)
	return n, e
}
