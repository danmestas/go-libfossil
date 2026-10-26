
.PHONY: test test-drivers test-otel test-all vet build setup-hooks docs-gen-sdk docs-gen-llms docs-check-llms docs-gen docs-check docs-version docs-serve docs-build docs

test:
	go test ./... -count=1 -timeout=120s

test-drivers:
	@echo "=== modernc (default) ==="
	go test ./... -count=1 -timeout=120s
	@echo "=== ncruces ==="
	go test -tags test_ncruces ./... -count=1 -timeout=120s
	@echo "=== all drivers passed ==="

test-otel:
	cd observer/otel && GOWORK=off go test ./... -count=1

test-all: test-drivers test-otel build
	@echo "=== all targets passed ==="

vet:
	go vet ./...

build:
	go build ./cmd/libfossil/

setup-hooks:
	git config core.hooksPath .githooks
	@echo "Pre-commit hook installed. Runs both drivers + DST + OTel (~45s) before each commit."
	@echo "Skip with: git commit --no-verify"

docs-gen-sdk:
	./scripts/gen-sdk-docs.sh

# llms.txt and llms-full.txt, from docs/site/llms-header.md and the content tree.
docs-gen-llms:
	./scripts/gen-llms-txt.sh

docs-check-llms:
	./scripts/check-llms-txt.sh

# Every generated docs input: the hero figure and conformance data, the CLI
# reference and llms*.txt. docs-check fails if any is stale; CI runs it.
docs-gen:
	./scripts/gen-docs.sh

docs-check: docs-check-llms
	./scripts/check-docs-generated.sh

# params.version and params.goVersion in docs/site/hugo.yaml, from the latest
# release tag and go.mod. The Cloudflare build runs it before Hugo.
docs-version:
	./scripts/sync-version.sh

docs-serve:
	cd docs/site && GOWORK=off hugo server -D --port 1313

# The deploy build (scripts/docs-build.sh, no Go needed) after regenerating
# every generated input.
docs-build: docs-gen-sdk docs-gen
	GOWORK=off ./scripts/docs-build.sh

docs: docs-build
	@echo "=== docs: built into docs/site/public/ ==="

# CI mirror — must match .github/workflows/test.yml verbatim across all jobs.
# Note: GOWORK=off is critical; the existing test/test-drivers/test-otel
# targets do NOT use it but CI does, leading to historical drift. These
# ci-* targets are the single source of truth going forward.
.PHONY: ci ci-default ci-ncruces ci-otel-target

ci: ci-default ci-ncruces ci-otel-target

ci-default:
	GOWORK=off go test $$(GOWORK=off go list ./... | grep -v '/dst') -count=1 -timeout=120s
	GOWORK=off go test ./dst/... -count=1 -timeout=300s
	cd db/driver/modernc && GOWORK=off go test ./... -count=1
	GOWORK=off go vet ./...
	GOWORK=off go build ./cmd/libfossil/

ci-ncruces:
	GOWORK=off go test -tags test_ncruces $$(GOWORK=off go list ./... | grep -v '/dst') -count=1 -timeout=120s
	GOWORK=off go test -tags test_ncruces ./dst/... -count=1 -timeout=300s
	cd db/driver/ncruces && GOWORK=off go test ./... -count=1

ci-otel-target:
	cd observer/otel && GOWORK=off go test ./... -count=1

.PHONY: release
release:
	@test -n "$(VERSION)" || { echo "VERSION=vX.Y.Z required"; exit 1; }
	@echo "$(VERSION)" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(-.+)?$$' || { echo "bad version format: $(VERSION)"; exit 1; }
	@git diff --quiet || { echo "tree dirty; commit or stash first"; exit 1; }
	@git fetch origin --tags
	@if git rev-parse "$(VERSION)" >/dev/null 2>&1; then echo "tag $(VERSION) already exists"; exit 1; fi
	@$(MAKE) ci
	@PREV=$$(git describe --tags --abbrev=0 2>/dev/null || echo ""); \
	  TMPL=.github/RELEASE_TEMPLATE.md; \
	  TMP=$$(mktemp); \
	  { echo "Release $(VERSION)"; echo; \
	    [ -f $$TMPL ] && { cat $$TMPL; echo; }; \
	    echo "## Changes"; \
	    if [ -n "$$PREV" ]; then git log --oneline $$PREV..HEAD; else git log --oneline; fi; } > $$TMP; \
	  $${EDITOR:-vi} $$TMP; \
	  git tag -a "$(VERSION)" -F $$TMP; \
	  rm $$TMP
	@echo ""
	@echo "Tag $(VERSION) created locally. To publish:"
	@echo "  git push origin $(VERSION)"
