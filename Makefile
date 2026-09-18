.PHONY: build build-linux run-observe run-shadow dump check test race vet fmt fmt-check tidy clean

build:
	go build -o bin/bot ./cmd/bot

build-linux:
	GOOS=linux GOARCH=amd64 go build -o bin/bot ./cmd/bot

build-all:
	go build -o bin/ ./cmd/...

run-observe:
	go run ./cmd/bot -pair xrp_jpy -mode observe -print-state

run-shadow:
	go run ./cmd/bot -pair xrp_jpy -mode shadow -model jev-1.13.0 -log-dir ./data

# Raw frames from the exchange, for re-checking the stream contract.
dump:
	go run ./cmd/dump -pair xrp_jpy -for 20s

# What must pass before a commit.
check: vet fmt-check race

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

fmt-check:
	@unformatted=$$(gofmt -s -l . | grep -v '^$$' || true); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt -s needed:"; echo "$$unformatted"; exit 1; \
	fi

tidy:
	go mod tidy

clean:
	rm -rf bin/
