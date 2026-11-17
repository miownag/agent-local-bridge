package main

import (
	"context"
	"errors"
	"flag"
	"github.com/miownag/agent-local-bridge/internal/bridge"
	"github.com/miownag/agent-local-bridge/internal/worker"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	env := flag.String("env", "", "optional literal environment file")
	endpoint := flag.String("server", "", "HTTPS server origin (or BRIDGE_PUBLIC_URL)")
	root := flag.String("root", "", "existing dedicated test directory (required)")
	allow := flag.Bool("allow-http", false, "allow HTTP on loopback for local testing")
	poll := flag.Duration("poll", 2*time.Second, "task polling interval")
	flag.Parse()
	if e := bridge.LoadEnv(*env); e != nil {
		log.Fatal(e)
	}
	if *endpoint == "" {
		*endpoint = os.Getenv("BRIDGE_PUBLIC_URL")
	}
	if *root == "" {
		log.Fatal("--root must select a dedicated test directory")
	}
	w, e := worker.New(worker.Config{ServerURL: *endpoint, Token: os.Getenv("BRIDGE_WORKER_TOKEN"), Root: *root, AllowHTTP: *allow, PollInterval: *poll})
	if e != nil {
		log.Fatal(e)
	}
	defer w.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if e = w.Run(ctx); e != nil && !errors.Is(e, context.Canceled) {
		log.Fatal(e)
	}
}
