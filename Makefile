# mailstatus-collector
#   make help            list targets
#   On a server, from the extracted bundle: sudo ./install.sh
BINARY  := mailstatus-collector
PKG     := ./cmd/$(BINARY)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || date -u +%Y%m%d%H%M%S)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath

.PHONY: help build dist test test-integration cover lint fmt install clean

help:                 ## show this help
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

build:                ## static linux binary in ./bin (GOARCH=arm64 for ARM)
	CGO_ENABLED=0 GOOS=linux GOARCH=$${GOARCH:-amd64} go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(PKG)

dist: build           ## release bundle dist/mailstatus-collector-<version>-linux-<arch>.tar.gz (never includes .env files)
	@arch=$${GOARCH:-amd64}; name=$(BINARY)-$(VERSION)-linux-$$arch; \
	rm -rf dist/$$name && mkdir -p dist/$$name && \
	cp -r bin deploy docs install.sh README.md LICENSE dist/$$name/ && \
	find dist/$$name -name '.env' -delete && \
	(cd dist/$$name && sha256sum bin/$(BINARY) > SHA256SUMS) && \
	tar -C dist -czf dist/$$name.tar.gz $$name && rm -rf dist/$$name && \
	echo "dist/$$name.tar.gz"

test:                 ## unit tests with the race detector (no broker needed)
	go test -race ./...

test-integration:     ## also run tests against a throw-away broker: MAILSTATUS_TEST_RABBITMQ_URL
	@test -n "$$MAILSTATUS_TEST_RABBITMQ_URL" || { echo "set MAILSTATUS_TEST_RABBITMQ_URL (e.g. amqp://guest:guest@127.0.0.1:5672/)"; exit 1; }
	go test -race -count=1 ./...

cover:                ## unit tests with a coverage report in coverage.html
	go test -coverprofile=coverage.out ./... && go tool cover -html=coverage.out -o coverage.html

lint:                 ## gofmt + go vet (+ staticcheck when installed)
	@out=$$(gofmt -l .); test -z "$$out" || { echo "gofmt needed:"; echo "$$out"; exit 1; }
	go vet ./...
	@if command -v staticcheck >/dev/null; then staticcheck ./...; fi

fmt:                  ## format the code
	gofmt -w .

install:              ## install from bin/ (run as root on the server; same as ./install.sh)
	./install.sh

clean:                ## remove build output
	rm -rf bin dist coverage.out coverage.html
