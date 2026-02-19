# Run from the repo root: make -C docker [target]
# Or from within this directory: make [target]
# Build context is always the repository root.

IMAGE_NAME := sticky-overseer
REPO_ROOT  := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/..)
VERSION    ?= $(shell cat $(REPO_ROOT)/VERSION)
DIST_DIR   := dist

.PHONY: build export load clean help version

help: ## Show this help message
	@echo "Usage: make -C docker [target] [VERSION=x.y.z]"
	@echo ""
	@echo "Current version : $(VERSION)"
	@echo "Image tag       : $(IMAGE_NAME):$(VERSION)"
	@echo ""
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-10s %s\n", $$1, $$2}'

version: ## Show current version
	@echo $(VERSION)

build: ## Build the sticky-overseer image
	docker build \
		-f $(CURDIR)/Dockerfile \
		-t $(IMAGE_NAME):$(VERSION) \
		-t $(IMAGE_NAME):latest \
		$(REPO_ROOT)
	@echo "Build complete: $(IMAGE_NAME):$(VERSION)"

export: build ## Build and export image to docker/dist/
	@mkdir -p $(DIST_DIR)
	docker save -o $(DIST_DIR)/$(IMAGE_NAME)-$(VERSION).tar $(IMAGE_NAME):$(VERSION) $(IMAGE_NAME):latest
	@echo "Exported $(DIST_DIR)/$(IMAGE_NAME)-$(VERSION).tar  ($$(du -h $(DIST_DIR)/$(IMAGE_NAME)-$(VERSION).tar | cut -f1))"

load: ## Load a previously exported tar. Usage: make load FILE=dist/sticky-overseer-*.tar
	@if [ -z "$(FILE)" ]; then \
		echo "Error: FILE is required. Usage: make load FILE=dist/sticky-overseer-*.tar"; \
		exit 1; \
	fi
	docker load -i $(FILE)
	@echo "Image loaded from $(FILE)"

clean: ## Remove sticky-overseer images and dist/
	-docker images --format '{{.Repository}}:{{.Tag}}' | grep '^sticky-overseer' | xargs -r docker rmi 2>/dev/null || true
	-rm -rf $(DIST_DIR)

.DEFAULT_GOAL := help
