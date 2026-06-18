.PHONY: build hub watcher test lint run tidy clean schema hooks

build: hub watcher

schema:
	bash scripts/gen-schema.sh


hub:
	CGO_ENABLED=0 go build -o bin/hub ./cmd/hub

watcher:
	CGO_ENABLED=0 go build -o bin/watcher ./cmd/watcher

test:
	go test -race ./...

lint:
	golangci-lint run

# One-time setup: point git at .githooks/ so the pre-push hook runs the same
# gates CI runs (lint, sqlfluff when SQL changed, schema drift when db/ changed).
# Skip a single push with: git push --no-verify
# Skip the whole hook with: CAW_SKIP_HOOKS=1 git push
hooks:
	git config core.hooksPath .githooks
	@echo "✓ git hooks installed (core.hooksPath = .githooks)"
	@echo "  pre-push will run: golangci-lint, go vet, sqlfluff (SQL changes), schema drift (db/ changes)"

run:
	go run ./cmd/hub

tidy:
	go mod tidy

clean:
	rm -rf bin
