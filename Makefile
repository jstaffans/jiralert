GO := go
GOBIN       ?= ${GOPATH}/bin
STATICCHECK := staticcheck

VERSION := $(shell git describe --tags 2>/dev/null)
ifeq "$(VERSION)" ""
VERSION := $(shell git rev-parse --short HEAD)
endif

RELEASE     := jiralert-$(VERSION).linux-amd64
RELEASE_DIR := release/$(RELEASE)

PACKAGES           := $(shell $(GO) list ./... | grep -v /vendor/)
DOCKER_IMAGE_NAME  := jiralert

# v1.2.0
ERRCHECK_VERSION  ?= e14f8d59a22d460d56c5ee92507cd94c78fbf274
ERRCHECK          ?= errcheck

all: check-go-mod clean format check errcheck build test

clean:
	@rm -rf jiralert release

format:
	@echo ">> formatting code"
	@$(GO) fmt $(PACKAGES)

check: 
	@echo ">> running staticcheck"
	@$(STATICCHECK) $(PACKAGES)

build:
	@echo ">> building binaries"
	@# CGO must be disabled to run in busybox container.
	@GO111MODULE=on CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -ldflags "-X main.Version=$(VERSION)" github.com/free/jiralert/cmd/jiralert

# docker builds docker with no tag.
docker: build
	@echo ">> building docker image '${DOCKER_IMAGE_NAME}'"
	@docker build -t "${DOCKER_IMAGE_NAME}" .

tarball:
	@echo ">> packaging release $(VERSION)"
	@rm -rf "$(RELEASE_DIR)/*"
	@mkdir -p "$(RELEASE_DIR)"
	@cp jiralert README.md LICENSE "$(RELEASE_DIR)"
	@mkdir -p "$(RELEASE_WDIR)/config"
	@cp config/* "$(RELEASE_DIR)/config"
	@tar -zcvf "$(RELEASE).tar.gz" -C "$(RELEASE_DIR)"/.. "$(RELEASE)"
	@rm -rf "$(RELEASE_DIR)"

.PHONY: check-go-mod
check-go-mod:
	@go mod verify

# errcheck performs static analysis and returns error if any of the errors is not checked.
.PHONY: errcheck
errcheck: 
	@echo ">> errchecking the code"
	$(ERRCHECK) -verbose -exclude .errcheck_excludes.txt ./cmd/... ./pkg/...

.PHONY: test
test:
	@echo ">> running all tests."
	@go test $(shell go list ./... | grep -v /vendor/);

