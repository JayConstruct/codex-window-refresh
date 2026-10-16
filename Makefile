GOCACHE ?= /tmp/codex-window-refresh-go-cache
export GOCACHE
export GOTOOLCHAIN := local

.PHONY: check test vet race build plugin native-smoke installer-check panel-check release-check clean v2-build v2-smoke

check: test vet

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

panel-check:
	node --test scripts/tests/panel*.test.cjs

release-check:
	python3 scripts/release_check.py

# The v2 prototype is built separately; release packaging remains v1.
v2-build:
	mkdir -p bin
	CGO_ENABLED=1 go build -buildvcs=false -o bin/window-refreshd ./cmd/window-refreshd
	cc -std=c11 -Wall -Wextra -Werror -fPIC -fvisibility=hidden -shared -o bin/codex-window-refresh-v2-bridge.so native/bridge/bridge.c native/bridge/events.c -pthread -lcurl -l:libjson-c.so.5

v2-smoke: v2-build
	python3 native/bridge/tests/smoke.py
	node --test native/bridge/tests/panel.test.cjs

clean:
	rm -rf bin dist scripts/__pycache__ scripts/tests/__pycache__
	rm -f *.test *.pprof coverage.out
