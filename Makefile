BINARY := mqw

.DEFAULT_GOAL := help
.PHONY: help build run test vet fmt check install release-check release-snapshot clean

help: ## Show this help
	@grep -hE '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sort | awk -F':.*## ' '{printf "  make %-17s %s\n", $$1, $$2}'
	@echo ""
	@echo "  Pass flags to run with ARGS, e.g."
	@echo "    make run ARGS=\"-repo acme/service\""

build: ## Build the binary
	go build -o $(BINARY) .

run: build ## Build and run; flags go in ARGS
	./$(BINARY) $(ARGS)

test: ## Run the tests
	go test ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format the source in place
	gofmt -w .

check: fmt vet test ## Format, vet and test

install: ## Build and install into GOBIN
	go install .

release-check: ## Validate the goreleaser config
	goreleaser check

release-snapshot: ## Build a local release without publishing
	goreleaser release --snapshot --clean --skip=publish

clean: ## Remove the binary and build output
	rm -rf $(BINARY) dist
