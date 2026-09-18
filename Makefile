.PHONY: help build build-linux build-all run-observe run-shadow dump logcheck preflight preflight-dry check test race vet fmt fmt-check tidy clean deploy

# `make help` lists what exists. If a target you expect is missing, the checkout
# is older than you think — see the phase order in CLAUDE.md.
help:
	@echo "jev-tick-lab targets:"
	@echo
	@echo "  running"
	@echo "    run-observe     phase 1: stream and state only, no model calls, no key"
	@echo "    run-shadow      phase 2: evaluate and log at the deployed 3s cadence"
	@echo "    dump            raw exchange frames, for re-checking the stream contract"
	@echo
	@echo "  before a long run"
	@echo "    preflight       one real API call, fully checked. Needs TYPESAFE_API_KEY"
	@echo "    preflight-dry   everything preflight does except the call. No key needed"
	@echo "    logcheck        is the collection still producing usable data?"
	@echo
	@echo "  building and checking"
	@echo "    build           bin/bot for this machine"
	@echo "    build-linux     bin/bot for the VM"
	@echo "    build-all       every command into bin/"
	@echo "    check           vet, gofmt check, go test -race  (run before committing)"
	@echo "    test race vet fmt fmt-check tidy clean"
	@echo
	@echo "  deploying"
	@echo "    deploy PROJECT=your-gcp-project"

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

# Is the collection still producing a usable dataset?
logcheck:
	go run ./cmd/logcheck -dir ./data -window 1h

# One real API call, checked end to end. Run before any long collection.
preflight:
	go run ./cmd/preflight -pair xrp_jpy -model jev-1.13.0

# Everything preflight does except the call; needs no API key.
preflight-dry:
	go run ./cmd/preflight -pair xrp_jpy -dry-run

# Ships binaries and units to the provisioned VM. See docs/operations.md.
deploy:
	./deploy/deploy.sh $(PROJECT)

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
