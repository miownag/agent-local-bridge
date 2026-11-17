package main

import (
	"context"
	"flag"
	"github.com/miownag/agent-local-bridge/internal/bridge"
	"github.com/miownag/agent-local-bridge/internal/server"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	env := flag.String("env", "", "optional literal environment file")
	listen := flag.String("listen", "127.0.0.1:8787", "HTTP listen address")
	flag.Parse()
	if e := bridge.LoadEnv(*env); e != nil {
		log.Fatal(e)
	}
	// The standalone server has no TLS listener; expose it through an HTTPS proxy.
	if os.Getenv("BRIDGE_DEV") == "1" {
		host, _, e := net.SplitHostPort(*listen)
		ip := net.ParseIP(host)
		if e != nil || ip == nil || !ip.IsLoopback() {
			log.Fatal("local development must listen on a loopback address")
		}
	}
	a, e := server.FromEnv()
	if e != nil {
		log.Fatal(e)
	}
	s := &http.Server{Addr: *listen, Handler: a, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 70 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 * 1024}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(c)
	}()
	log.Println("Bridge server listening on", *listen)
	if e = s.ListenAndServe(); e != nil && e != http.ErrServerClosed {
		log.Fatal(e)
	}
}
