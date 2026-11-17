package handler

import (
	"github.com/miownag/agent-local-bridge/internal/server"
	"net/http"
	"sync"
)

var once sync.Once
var app *server.App
var initErr error

// Handler is the Vercel Go Function entry point.
func Handler(w http.ResponseWriter, r *http.Request) {
	once.Do(func() { app, initErr = server.FromEnv() })
	if initErr != nil {
		http.Error(w, "Bridge configuration unavailable. Check server environment variables.", 503)
		return
	}
	if path := r.URL.Query().Get("bridge_path"); path != "" {
		copyURL := *r.URL
		copyURL.Path = path
		copyURL.RawPath = ""
		q := copyURL.Query()
		q.Del("bridge_path")
		copyURL.RawQuery = q.Encode()
		r = r.Clone(r.Context())
		r.URL = &copyURL
	}
	app.ServeHTTP(w, r)
}
