# Run all targets from the repo root: make -C docker [target]
# Or from within this directory: make [target]
# Build context is always the repository root.

# PROFILE selects which Dockerfile.<PROFILE> to build and determines image tags.
# Built-in profiles:
#   base     (default) — overseer only, no pinned command
#   recorder           — bundles sticky-recorder + ffmpeg; pins -command sticky-recorder
#
# Add any new profile by dropping a Dockerfile.<profile> into this directory.
# Example: make build PROFILE=ffmpeg  →  builds Dockerfile.ffmpeg, tags sticky-overseer-ffmpeg:*

PROFILE ?= base

IMAGE_NAME  := sticky-overseer
REPO_ROOT   := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/..)
VERSION     ?= $(shell cat $(REPO_ROOT)/VERSION)
DIST_DIR    := dist

# For non-base profiles the image is tagged sticky-overseer-<profile>:<version>
_TAG_SUFFIX = $(if $(filter base,$(PROFILE)),,$(addprefix -,$(PROFILE)))
_IMAGE_TAG  = $(IMAGE_NAME)$(_TAG_SUFFIX)
_DOCKERFILE = $(CURDIR)/Dockerfile.$(PROFILE)
_EXPORT_FILE = $(DIST_DIR)/$(_IMAGE_TAG)-$(VERSION).tar

# Extra build args forwarded to docker build (e.g. for recorder profile)
STICKY_RECORDER_REF  ?=
STICKY_RECORDER_REPO ?=
_RECORDER_REF_ARG    = $(if $(STICKY_RECORDER_REF),--build-arg STICKY_RECORDER_REF=$(STICKY_RECORDER_REF),)
_RECORDER_REPO_ARG   = $(if $(STICKY_RECORDER_REPO),--build-arg STICKY_RECORDER_REPO=$(STICKY_RECORDER_REPO),)

.PHONY: build export load clean help version

help: ## Show this help message
	@echo "Usage: make -C docker [target] [PROFILE=base|recorder|…] [VERSION=x.y.z]"
	@echo ""
	@echo "Current version : $(VERSION)"
	@echo "Active profile  : $(PROFILE)"
	@echo "Dockerfile      : $(_DOCKERFILE)"
	@echo "Image tag       : $(_IMAGE_TAG):$(VERSION)"
	@echo ""
	@echo "Available profiles (Dockerfile.<name> in this directory):"
	@ls $(CURDIR)/Dockerfile.* 2>/dev/null | sed 's|.*/Dockerfile\.||' | sed 's/^/  /'
	@echo ""
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-10s %s\n", $$1, $$2}'

version: ## Show current version
	@echo $(VERSION)

build: ## Build image for PROFILE (default: base). E.g. make build PROFILE=recorder
	@test -f $(_DOCKERFILE) || { echo "ERROR: $(_DOCKERFILE) not found"; exit 1; }
	@echo "Building $(_IMAGE_TAG):$(VERSION)  [Dockerfile.$(PROFILE)]"
	docker build \
		-f $(_DOCKERFILE) \
		-t $(_IMAGE_TAG):$(VERSION) \
		-t $(_IMAGE_TAG):latest \
		$(_RECORDER_REF_ARG) \
		$(_RECORDER_REPO_ARG) \
		$(REPO_ROOT)
	@echo "Build complete: $(_IMAGE_TAG):$(VERSION)"

export: build ## Build and export image to docker/dist/
	@echo "Exporting $(_IMAGE_TAG) ($(VERSION) + latest) → $(_EXPORT_FILE)"
	@mkdir -p $(DIST_DIR)
	docker save -o $(_EXPORT_FILE) $(_IMAGE_TAG):$(VERSION) $(_IMAGE_TAG):latest
	@echo "Exported $(_EXPORT_FILE)  ($$(du -h $(_EXPORT_FILE) | cut -f1))"

load: ## Load a previously exported tar. Usage: make load FILE=dist/sticky-overseer-*.tar
	@if [ -z "$(FILE)" ]; then \
		echo "Error: FILE is required. Usage: make load FILE=dist/sticky-overseer-*.tar"; \
		exit 1; \
	fi
	docker load -i $(FILE)
	@echo "Image loaded from $(FILE)"

clean: ## Remove all built sticky-overseer* images and dist/
	@echo "Cleaning..."
	-docker images --format '{{.Repository}}:{{.Tag}}' | grep '^sticky-overseer' | xargs -r docker rmi 2>/dev/null || true
	-rm -rf $(DIST_DIR)
	@echo "Done"

.DEFAULT_GOAL := help
