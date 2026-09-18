.PHONY: build run-observe run-shadow test vet fmt

build:
	go build -o bin/bot ./cmd/bot

build-linux:
	GOOS=linux GOARCH=amd64 go build -o bin/bot ./cmd/bot

run-observe:
	go run ./cmd/bot -pair xrp_jpy -mode observe

run-shadow:
	go run ./cmd/bot -pair xrp_jpy -mode shadow -model jev-1.13.0 -log-dir ./data

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .
