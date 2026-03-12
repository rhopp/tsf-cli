BINARY_NAME ?= ./bin/tsf.test
GINKGO_LABEL_FILTER ?= tsf-demo
GINKGO_FLAGS ?= --ginkgo.v --ginkgo.label-filter="$(GINKGO_LABEL_FILTER)"

export CGO_ENABLED := 0
export GOFLAGS := -mod=mod

.PHONY: build test run clean tidy

# Resolve dependencies
tidy:
	go mod tidy

# Compile the test binary
build: tidy
	mkdir -p $(dir $(BINARY_NAME))
	go test -c -o $(BINARY_NAME) ./cmd/

# Run tests directly via go test
test:
	go test -v ./cmd/ $(GINKGO_FLAGS)

# Run the pre-compiled test binary
run: build
	./$(BINARY_NAME) $(GINKGO_FLAGS)

clean:
	rm -rf $(dir $(BINARY_NAME))
