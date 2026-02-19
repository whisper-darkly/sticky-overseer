VERSION ?= $(shell cat VERSION)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
LDFLAGS := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT)"
PREFIX  ?= /usr/local

.PHONY: build install clean

build:
	@mkdir -p dist
	go build $(LDFLAGS) -o dist/sticky-overseer .

install: build
	install -d $(PREFIX)/bin
	install -m 755 dist/sticky-overseer $(PREFIX)/bin/sticky-overseer

clean:
	rm -rf dist
