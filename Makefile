# Release binaries for every platform a node can run on. The client uploads
# the matching one when it deploys a node over SSH (deploy.go looks in dist/).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
LDFLAGS := -s -w

.PHONY: build test dist clean

build:
	go build -ldflags "$(LDFLAGS)" -o agentconsole .

test:
	go vet ./... && go test -race ./...

dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o dist/agentconsole_$${os}_$${arch} . || exit 1; \
	done

clean:
	rm -rf dist agentconsole
