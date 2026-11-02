GO ?= go
BINARY ?= build/hostmcp

.PHONY: all build fmt vet lint test smoke vuln check clean

all: check

build:
	$(GO) build -trimpath -o $(BINARY) ./cmd/hostmcp

# Fails if any Go file needs gofmt.
fmt:
	@out="$$(gofmt -l cmd internal)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./...

test:
	$(GO) test -race -count=1 ./...

smoke: build
	python3 scripts/smoke.py --binary ./$(BINARY) --mode stdio

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

check: fmt vet lint test smoke

clean:
	rm -rf build dist
