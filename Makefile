VERSION ?= $(shell cat VERSION)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
LDFLAGS := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT)"
PREFIX  ?= /usr/local

.PHONY: build install clean test test-tools test-docker

test:
	go test -v -race ./...

test-tools:
	@echo "==> Installing test tools..."
	@if ! command -v websocat >/dev/null 2>&1; then \
		echo "    websocat not found — attempting install via cargo..."; \
		if command -v cargo >/dev/null 2>&1; then \
			cargo install websocat; \
		else \
			echo "    cargo not found; trying apt..."; \
			if command -v apt-get >/dev/null 2>&1; then \
				sudo apt-get install -y websocat 2>/dev/null || true; \
			fi; \
		fi; \
	else \
		echo "    websocat already installed: $$(websocat --version)"; \
	fi
	@if ! python3 -c "import websockets" 2>/dev/null; then \
		echo "    Installing python3-websockets..."; \
		pip3 install --user websockets 2>/dev/null || pip install --user websockets 2>/dev/null || true; \
	else \
		echo "    python3 websockets already installed."; \
	fi
	@echo "==> Test tools ready."

test-docker: build
	bash docker/test.sh

build:
	@mkdir -p dist
	go build $(LDFLAGS) -o dist/sticky-overseer ./cmd/sticky-overseer

install: build
	install -d $(PREFIX)/bin
	install -m 755 dist/sticky-overseer $(PREFIX)/bin/sticky-overseer

clean:
	rm -rf dist
