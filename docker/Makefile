# Run all targets from the repo root: make -C docker [target]
# Or from within this directory: make [target]
# Build context is always the repository root.

.PHONY: build export load clean help version

IMAGE_NAME            := sticky-overseer
REPO_ROOT             := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/..)
VERSION               ?= $(shell cat $(REPO_ROOT)/VERSION)
DIST_DIR              := dist
EXPORT_FILE           := $(DIST_DIR)/$(IMAGE_NAME)-$(VERSION).tar
STICKY_RECORDER_REF   ?=
STICKY_RECORDER_REPO  ?=

# Pass --build-arg only when the variable is set
_RECORDER_REF_ARG  = $(if $(STICKY_RECORDER_REF),--build-arg STICKY_RECORDER_REF=$(STICKY_RECORDER_REF),)
_RECORDER_REPO_ARG = $(if $(STICKY_RECORDER_REPO),--build-arg STICKY_RECORDER_REPO=$(STICKY_RECORDER_REPO),)

help: ## Show this help message
	@echo "Usage: make -C docker [target] [VERSION=x.y.z] [STICKY_RECORDER_REF=v1.2.3]"
	@echo ""
	@echo "Current version: $(VERSION)"
	@echo ""
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-15s %s\n", $$1, $$2}'

version: ## Show current version
	@echo $(VERSION)

build: ## Build Docker image (build context: repo root)
	@echo "Building Docker image: $(IMAGE_NAME):$(VERSION)"
	docker build \
		-f $(CURDIR)/Dockerfile \
		-t $(IMAGE_NAME):$(VERSION) \
		-t $(IMAGE_NAME):latest \
		$(_RECORDER_REF_ARG) \
		$(_RECORDER_REPO_ARG) \
		$(REPO_ROOT)
	@echo "Build complete"

export: build ## Build and export Docker image to docker/dist/
	@echo "Exporting Docker images ($(VERSION) and latest) to $(EXPORT_FILE)"
	@mkdir -p $(DIST_DIR)
	docker save -o $(EXPORT_FILE) $(IMAGE_NAME):$(VERSION) $(IMAGE_NAME):latest
	@echo "Images exported to $(EXPORT_FILE)"
	@echo "  Size: $$(du -h $(EXPORT_FILE) | cut -f1)"
	@echo "  Tags: $(IMAGE_NAME):$(VERSION), $(IMAGE_NAME):latest"

load: ## Load exported image (usage: make load FILE=dist/sticky-overseer-*.tar)
	@if [ -z "$(FILE)" ]; then \
		echo "Error: FILE variable is required"; \
		echo "Usage: make -C docker load FILE=dist/sticky-overseer-*.tar"; \
		exit 1; \
	fi
	@echo "Loading Docker image from $(FILE)"
	docker load -i $(FILE)
	@echo "Image loaded"

clean: ## Remove built images and dist/
	@echo "Cleaning up..."
	-docker rmi $(IMAGE_NAME):$(VERSION) $(IMAGE_NAME):latest 2>/dev/null || true
	-rm -rf $(DIST_DIR)
	@echo "Cleanup complete"

.DEFAULT_GOAL := help
