GO ?= go
BINARY := lares
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet fmt check release clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/lares

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

check: fmt vet test

release:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/lares-linux-amd64 ./cmd/lares
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/lares-linux-arm64 ./cmd/lares

clean:
	rm -rf $(BINARY) dist
