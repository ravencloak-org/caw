.PHONY: build test lint run tidy clean

build:
	CGO_ENABLED=0 go build -o bin/hub ./cmd/hub

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
