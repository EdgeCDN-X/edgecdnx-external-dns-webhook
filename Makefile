GO ?= go

.PHONY: build test vet

build:
	mkdir -p dist
	CGO_ENABLED=0 $(GO) build -trimpath -o dist/edgecdnx-external-dns-webhook .

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...
