GOCACHE ?= /tmp/codex-window-refresh-go-cache
export GOCACHE
export GOTOOLCHAIN := local

.PHONY: check docs-check test vet race build plugin native-smoke installer-check panel-check v1-panel-check v2-panel-check release-check v1-release-check v2-release-check clean v2-build v2-smoke v2-panel-browser-check

check: docs-check test vet

docs-check:
	python3 scripts/docs_check.py

test:
	go test ./...

vet:
	go vet ./...

race:
	go test -race ./...

build:
	mkdir -p bin
	go build -buildvcs=false -o bin/window-refresh ./cmd/window-refresh

plugin:
	mkdir -p bin
	CGO_ENABLED=1 go build -buildvcs=false -buildmode=c-shared -o bin/codex-window-refresh.so ./cmd/plugin

native-smoke: plugin
	sh scripts/native-smoke.sh

installer-check: build
	python3 -m unittest discover -s scripts/tests -v

panel-check: v1-panel-check v2-panel-check

v1-panel-check:
	node --test tests/web/v1/*.test.cjs

v2-panel-check:
	node --test tests/web/v2/*.test.cjs

release-check: v1-release-check v2-release-check

v1-release-check:
	python3 scripts/release_check.py

v2-release-check:
	python3 scripts/package-v2.py --check

# Build and package v2 independently from v1.
v2-build:
	mkdir -p bin
	CGO_ENABLED=1 go build -buildvcs=false -o bin/window-refreshd ./cmd/window-refreshd
	cc -std=c11 -Wall -Wextra -Werror -fPIC -fvisibility=hidden -shared -o bin/codex-window-refresh-v2-bridge.so native/bridge/bridge.c native/bridge/events.c native/bridge/scheduler.c -pthread -lcurl -l:libjson-c.so.5

v2-smoke: v2-build v2-panel-check
	python3 native/bridge/tests/smoke.py

v2-panel-browser-check:
	node scripts/v2-panel-browser-check.mjs

clean:
	rm -rf bin dist scripts/__pycache__ scripts/tests/__pycache__
	rm -f *.test *.pprof coverage.out
