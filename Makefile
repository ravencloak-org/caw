.PHONY: build hub watcher test lint run tidy clean

build: hub watcher

hub:
	CGO_ENABLED=0 go build -o bin/hub ./cmd/hub

watcher:
	CGO_ENABLED=0 go build -o bin/watcher ./cmd/watcher

test:
	go test -race ./...

lint:
	golangci-lint run

run:
	go run ./cmd/hub

tidy:
	go mod tidy

clean:
	rm -rf bin
