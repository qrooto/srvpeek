GO ?= go
FLAGS = -buildvcs=false -trimpath -ldflags='-s -w'

.PHONY: build release test
build:
	mkdir -p dist
	CGO_ENABLED=0 $(GO) build $(FLAGS) -o dist/srvpeek .

release:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(FLAGS) -o dist/srvpeek-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build $(FLAGS) -o dist/srvpeek-linux-arm64 .
	cd dist && sha256sum srvpeek-linux-amd64 srvpeek-linux-arm64 > SHA256SUMS

test:
	$(GO) test ./...
	$(GO) vet ./...
