.PHONY: build clean test

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_TIME ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS := -ldflags "-X github.com/marccampbell/autoprobe/cmd.Version=$(VERSION) \
	-X github.com/marccampbell/autoprobe/cmd.GitCommit=$(GIT_COMMIT) \
	-X github.com/marccampbell/autoprobe/cmd.BuildTime=$(BUILD_TIME)"

build:
	go build $(LDFLAGS) -o autoprobe .

clean:
	rm -f autoprobe

test:
	go test -v ./...

install:
	go install $(LDFLAGS) .
