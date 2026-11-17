.PHONY: test build
test:
	go test -race ./...
	go vet ./...

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -o bin/bridge-server ./cmd/server
	CGO_ENABLED=0 go build -trimpath -o bin/bridge-worker ./cmd/worker
