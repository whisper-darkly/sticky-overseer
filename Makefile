VERSION ?= 0.1.0
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
LDFLAGS := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT)"
PREFIX  ?= /usr/local

.PHONY: build install clean

build:
	@mkdir -p dist
	go build $(LDFLAGS) -o dist/sticky_overseer .

install: build
	install -d $(PREFIX)/bin
	install -m 755 dist/sticky_overseer $(PREFIX)/bin/sticky_overseer

clean:
	rm -rf dist
