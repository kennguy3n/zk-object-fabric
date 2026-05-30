# zk-object-fabric — top-level convenience targets.
#
# Most day-to-day commands are just `go test ./...` / `go build ./...`.
# This Makefile exists for the artifacts that take more than one
# command to produce, primarily the external-audit hand-off bundle.

GO        ?= go
GIT       ?= git
SHELL     := /usr/bin/env bash

# Resolved at make-time so the bundle name records the SHA of the
# commit that produced it.
COMMIT    := $(shell $(GIT) rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE      := $(shell date -u +%Y%m%d)
BUILD_DIR := build/audit

# Pick a SHA-256 hasher that exists on this box. GNU coreutils
# ships `sha256sum` (Linux + many CI images); macOS / BSD ship
# `shasum -a 256`. Both emit the same `<hex>  <path>` line so the
# downstream MANIFEST.txt format is identical either way. Detect at
# make-time rather than at recipe-time so the recipe stays readable
# and we fail loudly if neither is on PATH.
SHA256    := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || (command -v shasum >/dev/null 2>&1 && echo "shasum -a 256"))
ifeq ($(strip $(SHA256)),)
$(error neither sha256sum nor shasum is on PATH; install GNU coreutils (Linux: sudo apt install coreutils; macOS: brew install coreutils) or perl-shasum)
endif
# Go's ./... walker ignores directories whose name starts with `_`
# or `.`, so the staging tree is deliberately prefixed with `_` to
# keep `go build ./...` / `go vet ./...` from re-discovering the
# copied source files as a duplicate package.
STAGING   := $(BUILD_DIR)/_staging
BUNDLE    := $(BUILD_DIR)/zkof-audit-$(DATE)-$(COMMIT).tar.gz

.PHONY: help
help:
	@echo "Targets:"
	@echo "  audit-bundle   Build the external-audit tarball for hand-off."
	@echo "  audit-verify   Re-run static analysis and emit the reports the bundle ships with."
	@echo "  audit-clean    Remove the build/audit directory."
	@echo "  test           Run the full test suite with -race."
	@echo "  vet            go vet ./..."

.PHONY: test
test:
	$(GO) test -race -count=1 ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: audit-clean
audit-clean:
	rm -rf $(BUILD_DIR)

# audit-verify runs the static-analysis baseline the audit bundle
# embeds. We keep it as its own target so a reviewer can re-run it
# against an in-flight branch without rebuilding the tarball.
.PHONY: audit-verify
audit-verify:
	@mkdir -p $(BUILD_DIR)/reports
	@echo "==> go vet ./..."
	@$(GO) vet ./... > $(BUILD_DIR)/reports/go-vet.txt 2>&1 || (cat $(BUILD_DIR)/reports/go-vet.txt; exit 1)
	@echo "==> govulncheck ./... (best-effort; absent in some environments)"
	@if command -v govulncheck >/dev/null 2>&1; then \
		govulncheck ./... > $(BUILD_DIR)/reports/govulncheck.txt 2>&1 || true; \
	else \
		echo "govulncheck not installed; skipping" > $(BUILD_DIR)/reports/govulncheck.txt; \
	fi
	@echo "==> staticcheck ./... (best-effort; absent in some environments)"
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./... > $(BUILD_DIR)/reports/staticcheck.txt 2>&1 || true; \
	else \
		echo "staticcheck not installed; skipping" > $(BUILD_DIR)/reports/staticcheck.txt; \
	fi
	@echo "Static-analysis reports written to $(BUILD_DIR)/reports/"

# audit-bundle assembles a self-contained tarball that an external
# auditor can read end-to-end without needing access to the repo.
# It includes:
#   - The three audit documents (security / crypto / threat-model)
#   - The exact source files cross-referenced from those documents
#     (so a finding can quote "wrap.go:74-78" without the auditor
#     having to clone the repo to look it up)
#   - The static-analysis reports from `audit-verify`
#   - A MANIFEST listing every file with its SHA-256 + the commit
#     SHA the bundle was built from
#
# The set of source files included is hard-coded below; it tracks the
# `path:line` references in the audit packages. When a new reference
# is added to either audit package, append the file here.
.PHONY: audit-bundle
audit-bundle: audit-verify
	@echo "==> Assembling audit bundle for commit $(COMMIT)"
	@mkdir -p $(STAGING)
	@rm -rf $(STAGING)/*
	@# Copy the three audit documents.
	@mkdir -p $(STAGING)/docs/security
	@cp docs/security/README.md \
	    docs/security/threat-model.md \
	    docs/security/audit-package-security.md \
	    docs/security/audit-package-cryptography.md \
	    $(STAGING)/docs/security/
	@if [ -d docs/security/findings ]; then \
		cp -R docs/security/findings $(STAGING)/docs/security/; \
	fi
	@# Copy the cross-referenced source files. Each one is cited by
	@# at least one of the audit documents.
	@mkdir -p $(STAGING)/sources
	@for f in \
		cmd/gateway/main.go \
		api/s3compat/handler.go \
		api/s3compat/encryption.go \
		api/s3compat/encryption_pipeline.go \
		internal/auth/authenticator.go \
		internal/auth/rate_limit.go \
		internal/auth/abuse.go \
		internal/auth/ddos_shield.go \
		internal/auth/tenant_store.go \
		internal/auth/legal_response.go \
		encryption/envelope.go \
		encryption/client_sdk/sdk.go \
		encryption/client_sdk/keygen.go \
		encryption/client_sdk/wrap.go \
		encryption/client_sdk/kms_wrapper.go \
		encryption/client_sdk/vault_wrapper.go \
		metadata/manifest_store/postgres/store.go \
		metadata/manifest_store/postgres/body_encryptor.go ; do \
		if [ -f "$$f" ]; then \
			mkdir -p "$(STAGING)/sources/$$(dirname $$f)"; \
			cp "$$f" "$(STAGING)/sources/$$f"; \
		else \
			echo "WARN: $$f referenced by audit docs but missing"; \
		fi; \
	done
	@# Embed the static-analysis reports produced by audit-verify.
	@mkdir -p $(STAGING)/reports
	@cp $(BUILD_DIR)/reports/*.txt $(STAGING)/reports/
	@# MANIFEST.txt records SHA-256 of every file shipped plus the
	@# commit SHA. An auditor can verify integrity against this file
	@# alone (e.g. `cd unpacked && shasum -a 256 -c MANIFEST.txt`).
	@#
	@# The hashing pipeline uses null-separated paths end-to-end
	@# (find -print0 -> sort -z -> xargs -0 $(SHA256)) rather than
	@# the older `find | sort | xargs -I{} sh -c '... {}'` pattern,
	@# which substituted the filename literally into a shell command
	@# and so broke -- or worse, allowed command injection -- on any
	@# path containing a space, a single quote, a `$$`, a backtick,
	@# or a newline. The audit bundle filenames are controlled today
	@# (Go sources under repo-controlled paths) but the safe pattern
	@# costs nothing and removes a class of future foot-gun. The
	@# hasher ($(SHA256), either `sha256sum` or `shasum -a 256`)
	@# emits `<hex>  <path>` lines directly, so no manual printf-
	@# rebuilding is needed and the format stays identical across
	@# Linux and macOS.
	@( cd $(STAGING) && \
	    { \
	        echo "zk-object-fabric audit bundle"; \
	        echo "commit: $(COMMIT)"; \
	        echo "built:  $$(date -u +%Y-%m-%dT%H:%M:%SZ)"; \
	        echo ""; \
	        echo "sha256  path"; \
	        find . -type f ! -name MANIFEST.txt -print0 \
	          | LC_ALL=C sort -z \
	          | xargs -0 $(SHA256); \
	    } > MANIFEST.txt )
	@tar -czf $(BUNDLE) -C $(STAGING) .
	@echo "==> Wrote $(BUNDLE)"
	@ls -lh $(BUNDLE)
