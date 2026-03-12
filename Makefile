APP = tsf

BIN_DIR ?= ./bin
BIN ?= $(BIN_DIR)/$(APP)
IMAGE_FQN ?= tsf:latest

# Primary source code directories.
CMD ?= ./cmd

# Golang general flags for build and testing.
GOFLAGS ?= -v
GOFLAGS_TEST ?= -failfast -v -cover
CGO_ENABLED ?= 0
CGO_LDFLAGS ?= 

# Determine the appropriate tar command based on the operating system.
UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
	TAR := gtar
else
	TAR := tar
endif

# Directory with the installer resources, scripts, Helm Charts, etc.
INSTALLER_DIR ?= ./installer
# Tarball with the installer resources.
INSTALLER_TARBALL ?= $(INSTALLER_DIR)/installer.tar
# Data to include in the tarball.
INSTALLER_TARBALL_DATA ?= $(shell find -L $(INSTALLER_DIR) -type f \
	! -path "$(INSTALLER_TARBALL)" \
	! -name embed.go \
)

# Version will be set at build time via git describe
VERSION ?= $(shell \
	if [ -n "$(GITHUB_REF_NAME)" ]; then echo "${GITHUB_REF_NAME}"; \
	else git describe --tags --always || echo "v0.0.0-SNAPSHOT"; \
	fi)

# Commit will be set at build time via git commit hash
COMMIT_ID ?= $(shell git rev-parse HEAD)

.EXPORT_ALL_VARIABLES:

.DEFAULT_GOAL := build

#
# Help
#

# Lists available targets with descriptions (default target is marked with *).
.PHONY: help
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  build*                 Builds the application executable with installer resources embedded (default)"
	@echo "  debug                  Builds the application executable with debugging enabled"
	@echo "  run                    Runs the application (use ARGS=... for arguments)"
	@echo "  installer-tarball      Creates a tarball with all resources required for installation"
	@echo "  image                  Builds the container image (uses Podman by default)"
	@echo "  image-podman           Builds the container image with Podman"
	@echo "  lint                   Runs golangci-lint on the codebase"
	@echo ""
	@echo "  * = default when running 'make' with no target"

#
# Build and Run
#

# Builds the application executable with installer resources embedded.
.PHONY: $(BIN)
$(BIN): installer-tarball
$(BIN):  
	@echo "# Building '$(BIN)'"
	@[ -d $(BIN_DIR) ] || mkdir -p $(BIN_DIR)
	go build -ldflags "-X main.version=$(VERSION) -X main.commitID=$(COMMIT_ID)" -o $(BIN) $(CMD)

.PHONY: build
build: $(BIN)

# Builds the application executable with debugging enabled.
.PHONY: debug
debug: GOFLAGS = "-gcflags=all=-N -l"
debug: $(BIN)

# Runs the application with arbitrary ARGS.
.PHONY: run
run: installer-tarball
	go run $(CMD) $(ARGS)

#
# Installer Tarball
#

# Creates a tarball with all resources required for the installation process.
.PHONY: installer-tarball
installer-tarball: $(INSTALLER_TARBALL)
$(INSTALLER_TARBALL): $(INSTALLER_TARBALL_DATA)
	@echo "# Generating '$(INSTALLER_TARBALL)'"
	@test -f "$(INSTALLER_TARBALL)" && rm -f "$(INSTALLER_TARBALL)" || true
	@$(TAR) -C "$(INSTALLER_DIR)" -cpf "$(INSTALLER_TARBALL)" \
	$(shell echo "$(INSTALLER_TARBALL_DATA)" | sed "s:\./installer/:./:g")

#
# Container Image
#

# By default builds the container image using Podman.
image: image-podman

# Builds the container image with Podman.
image-podman:
	@echo "# Building '$(IMAGE_FQN)'..."
	podman build --build-arg COMMIT_ID=$(COMMIT_ID) --build-arg VERSION_ID=$(VERSION) --tag="$(IMAGE_FQN)" .

#
# Lint
#

# Uses golangci-lint to inspect the code base.
.PHONY: lint
lint:
	@which golangci-lint &>/dev/null || \
		go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest &>/dev/null
	golangci-lint run ./...
# test change
