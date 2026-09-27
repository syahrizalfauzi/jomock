.PHONY: run build test vet fmt

run:
	go run .

build:
	go build -o bin/jomock .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .
