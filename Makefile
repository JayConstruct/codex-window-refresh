GOCACHE ?= /tmp/codex-window-refresh-go-cache
export GOCACHE
export GOTOOLCHAIN := local

.PHONY: check test vet race build plugin native-smoke installer-check panel-check release-check clean

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

clean:
	rm -rf bin dist scripts/__pycache__ scripts/tests/__pycache__
	rm -f *.test *.pprof coverage.out
