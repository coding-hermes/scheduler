.PHONY: build test test-full run install clean lint fmt migrate deploy deploy-install release-prep tag

# Build identity (KB-GAP-040 class fix): every build target must pass
# $(LDFLAGS) — bare `go build` falls back to the vcs buildinfo resolver in
# internal/version, so even ldflags-less builds never report a stale version.
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
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
	go build -ldflags "$(LDFLAGS)" -o bin/schedulerd ./cmd/schedulerd/
	go build -ldflags "$(LDFLAGS)" -o bin/migrate ./cmd/migrate/

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
