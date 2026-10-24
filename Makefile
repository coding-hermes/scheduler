.PHONY: build test test-full run install clean lint fmt migrate deploy deploy-install release-prep tag

# Build identity: use `make build` as the canonical daemon build path. Bare
# `go build` may stamp an enclosing repository's VCS metadata in a linked
# worktree. The target injects identity resolved from this product checkout.
# A tagless checkout must still expose a parseable version; an archive has no
# git directory, so COMMIT also accepts the export-subst marker in
# internal/version/archive_commit.txt.
DESCRIBE   := $(shell git -C "$(CURDIR)" describe --tags --always --dirty 2>/dev/null || true)
COMMIT     := $(shell git -C "$(CURDIR)" rev-parse --short=8 HEAD 2>/dev/null || grep -oE '[0-9a-fA-F]{8,40}' "$(CURDIR)/internal/version/archive_commit.txt" 2>/dev/null | head -1 | cut -c1-8)
VERSION    := $(shell if git -C "$(CURDIR)" tag --merged HEAD --list 2>/dev/null | grep -q .; then printf '%s' "$(DESCRIBE)"; else printf 'dev-%s' "$(COMMIT)"; fi)
BINDIR     ?= bin
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w \
  -X github.com/coding-hermes/scheduler/internal/version.Version=$(VERSION) \
  -X github.com/coding-hermes/scheduler/internal/version.Commit=$(COMMIT) \
  -X github.com/coding-hermes/scheduler/internal/version.BuildDate=$(BUILD_DATE)

# Release tooling (RELEASE-004). `release-prep` is the version-authority step:
# it promotes CHANGELOG.md's [Unreleased] section into a `## [X.Y.Z] — <date>`
# heading and prints the next commands WITHOUT running them. VERSION is passed
# on the command line (`make release-prep VERSION=v1.4.0`) and therefore
# overrides the build-identity VERSION above for that invocation — release-prep
# compiles nothing, so there is no interaction with $(LDFLAGS).
#
# `tag` is the separate, explicitly-typed command that creates and pushes the
# annotated tag — deliberately NOT implied by release-prep, because a prep step
# that tagged and pushed on invocation is how a wrong tag gets published. It
# re-checks the two preconditions that matter for a tag at the moment of the tag.
#
# Full operator procedure: docs/releases.md
release-prep:
	@test -n "$(VERSION)" || { echo "usage: make release-prep VERSION=vX.Y.Z"; exit 2; }
	@./scripts/release-prep.sh $(VERSION)

tag:
	@test -n "$(VERSION)" || { echo "usage: make tag VERSION=vX.Y.Z"; exit 2; }
	@test "$$(git rev-parse HEAD)" = "$$(git rev-parse origin/main)" || { echo "refusing: HEAD != origin/main"; exit 2; }
	@test -z "$$(git status --porcelain)" || { echo "refusing: working tree is not clean"; exit 2; }
	git tag -a $(VERSION) -m 'Release $(VERSION)'
	git push origin $(VERSION)

build:
	@test -n "$(COMMIT)" || { echo "cannot determine product checkout commit for build identity" >&2; exit 2; }
	@if git -C "$(CURDIR)" rev-parse --git-dir >/dev/null 2>&1; then git -C "$(CURDIR)" cat-file -e "$(COMMIT)^{commit}" || { echo "product build identity does not resolve to a commit" >&2; exit 2; }; fi
	mkdir -p "$(BINDIR)"
	go build -ldflags "$(LDFLAGS)" -o "$(BINDIR)/schedulerd" ./cmd/schedulerd/
	go build -ldflags "$(LDFLAGS)" -o "$(BINDIR)/migrate" ./cmd/migrate/

test:
	go test -short -count=1 ./...

test-full:
	go test -count=1 ./...

run: build
	./bin/schedulerd

install:
	go install -ldflags "$(LDFLAGS)" ./...

clean:
	rm -rf bin/

lint:
	go vet ./...

fmt:
	gofmt -w .

migrate: build
	./bin/migrate -jobs $(HOME)/.hermes/cron/jobs.json -db $(HOME)/.hermes/coding-hermes/scheduler.db

migrate-dry: build
	./bin/migrate -jobs $(HOME)/.hermes/cron/jobs.json -db $(HOME)/.hermes/coding-hermes/scheduler.db --dry-run

deploy-install:
	sudo cp deploy/coding-hermes-scheduler.service /etc/systemd/system/
	sudo systemctl daemon-reload
	sudo systemctl enable coding-hermes-scheduler

deploy: build deploy-install
	sudo systemctl restart coding-hermes-scheduler
	systemctl status coding-hermes-scheduler --no-pager
