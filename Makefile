.PHONY: build clean test

build:
	go build -o bin/pr-review-parser ./cmd/pr-review-parser
	go build -o bin/pr-review-reply ./cmd/pr-review-reply

clean:
	rm -rf bin/

test:
	go test ./...
