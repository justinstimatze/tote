VERSION := $(shell git describe --tags --always --dirty)

.PHONY: build install test

build:
	go build -ldflags "-X main.version=$(VERSION)" -o tote .

install:
	go install -ldflags "-X main.version=$(VERSION)" .

test:
	go vet ./... && go test ./...
