.PHONY: build test vet fmt-check

build:
	go build ./...

test:
	go test -race ./...

vet:
	go vet ./...

fmt-check:
	@files=$$(gofmt -l .); \
	if [ -n "$$files" ]; then \
		echo "gofmt needed on:"; echo "$$files"; exit 1; \
	fi
