BINARY_NAME ?= fouskoti

.PHONY: build
build:
	go mod tidy
	CGO_ENABLED=0 go build -o ${BINARY_NAME}

.PHONY: test
test:
	go run github.com/onsi/ginkgo/v2/ginkgo -r ./...

.PHONY: test/noginkgo
test/noginkgo:
	go test -v ./...

.PHONY: lint
lint:
	go vet
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run

.PHONY: clean
clean:
	go clean
	rm ${BINARY_NAME}
