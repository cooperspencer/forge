.PHONY: build dist test run vet clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

# One binary is both the server (`forge serve`) and the runner (`forge runner`).
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/forge ./cmd/forge

# Cross-compile for every platform in PLATFORMS into dist/.
dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "dist/forge-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="$(LDFLAGS)" \
			-o dist/forge-$$os-$$arch$$ext ./cmd/forge || exit 1; \
	done

test:
	go test ./...

vet:
	go vet ./...

run: build
	./bin/forge serve

clean:
	rm -rf bin dist
