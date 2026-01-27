BINARY_NAME ?= fouskoti

.PHONY: build
build: ## Build the binary
	go mod tidy
	CGO_ENABLED=0 go build -o ${BINARY_NAME}

.PHONY: test
test: ## Run tests
	go run github.com/onsi/ginkgo/v2/ginkgo -r ./...

.PHONY: test/fast ## Run tests without the Ginkgo runner (faster, less detailed output)
test/fast:
	go test -v ./...

.PHONY: lint
lint: ## Lint the code
	go vet
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run

.PHONY: clean
clean: ## Clean build artifacts
	go clean
	rm ${BINARY_NAME}
