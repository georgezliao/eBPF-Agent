# The agent only runs on Linux. On macOS everything is cross-compiled and then run
# inside a Linux VM by scripts/linux.sh.
# bpf2go needs clang and llvm-strip with the bpf target. Apple's clang has no bpf
# backend, so Homebrew's LLVM goes first on PATH when it exists.
# The generated Go bindings and .o files are committed, so only `make generate` needs it.
LLVM ?= /opt/homebrew/opt/llvm/bin
GOARCH ?= $(shell go env GOARCH)
LINUX = GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0

.PHONY: generate generated build check itest clean

generate:
	PATH="$(LLVM):$$PATH" go generate ./...

# Fails if the committed BPF objects don't match bpf/*.c (needs clang).
generated: generate
	git diff --exit-code -- internal/probe

build:
	$(LINUX) go build -o bin/ ./cmd/...

check:
	$(LINUX) go vet ./...
	go test ./...

itest: build
	$(LINUX) go test -c -o bin/itest ./tests/integration
	scripts/linux.sh bin/itest -test.v $(ARGS)

clean:
	rm -rf bin
