.PHONY: build test test-full run install clean lint fmt migrate deploy deploy-install

build:
	go build -o bin/schedulerd ./cmd/schedulerd/
	go build -o bin/migrate ./cmd/migrate/

test:
	go test -short -count=1 ./...

test-full:
	go test -count=1 ./...

run: build
	./bin/schedulerd

install:
	go install ./...

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
