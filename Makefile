# The agent only runs on Linux. On macOS everything is cross-compiled and then run
# inside a privileged container by scripts/linux.sh.
CLANG ?= $(shell command -v /opt/homebrew/opt/llvm/bin/clang || command -v clang)
GOARCH ?= $(shell go env GOARCH)
LINUX = GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0

.PHONY: generate build check itest clean

generate:
	BPF_CLANG=$(CLANG) go generate ./...

build: generate
	$(LINUX) go build -o bin/ ./cmd/...

check:
	$(LINUX) go vet ./...
	go test ./...

itest: build
	$(LINUX) go test -c -o bin/itest ./tests/integration
	scripts/linux.sh bin/itest -test.v $(ARGS)

clean:
	rm -rf bin
