.PHONY: build test test-full run install clean lint fmt

build:
	go build -o bin/schedulerd ./cmd/schedulerd/

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
