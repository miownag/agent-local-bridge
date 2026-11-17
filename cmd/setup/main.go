package main

import (
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"os"
)

func secret() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		log.Fatal(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func main() {
	path := flag.String("out", ".env", "output file; must not already exist")
	flag.Parse()
	f, e := os.OpenFile(*path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		log.Fatal("cannot create environment file: ", e)
	}
	_, e = fmt.Fprintf(f, "BRIDGE_PUBLIC_URL=http://127.0.0.1:8787\nBRIDGE_DEV=1\nBRIDGE_ADMIN_SECRET=%s\nBRIDGE_WORKER_TOKEN=%s\nBRIDGE_NAMESPACE=agent-local-bridge\n", secret(), secret())
	closeErr := f.Close()
	if e != nil || closeErr != nil {
		log.Fatal("cannot finish writing environment file")
	}
	fmt.Println("Created", *path, "with owner-only permissions. Secrets are not printed.")
}
