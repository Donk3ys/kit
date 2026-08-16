.PHONY: check build vet fmt test cover examples tidy

# examples/ is a nested module, so `go test ./...` from here silently skips it.
# Use `make check` rather than remembering that.
check: fmt build vet test examples

build:
	go build ./...

vet:
	go vet ./...
	go -C examples vet ./...

# Fails if anything is unformatted, rather than reformatting silently.
fmt:
	@out="$$(gofmt -l . )"; \
	if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

test:
	go test ./... -count=1

cover:
	go test ./... -count=1 -cover

# The examples module: vet and test compile it, and every Example function
# asserts its own output. `go build ./...` cannot be used here — it would try
# to write a binary named "api" over the api/ directory.
examples:
	go -C examples vet ./...
	go -C examples test ./... -count=1

# Run the end-to-end walkthrough and print what a client actually sees.
walkthrough:
	go -C examples test ./api -run Example -v

tidy:
	go mod tidy
	go -C examples mod tidy
