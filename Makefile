BINARY      := eneverre
PKG         := eneverre
GO          := go
BUILD_DIR   := $(CURDIR)/dist
PKG_ROOT    := $(CURDIR)
GO_PKG_DIR  := $(PKG_ROOT)/go

GOOS        ?= $(shell $(GO) env GOOS 2>/dev/null || echo linux)
GOARCH      ?= $(shell $(GO) env GOARCH 2>/dev/null || echo amd64)
CGO_ENABLED ?= 0

VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
PKG_NAME    := $(PKG)-$(VERSION)-$(GOOS)-$(GOARCH)
STAGE_DIR   := $(BUILD_DIR)/$(PKG_NAME)

EXE :=
ifeq ($(GOOS),windows)
EXE := .exe
endif

LDFLAGS := -s -w -X main.version=$(VERSION)

RELEASE_TARGETS := linux/amd64 linux/arm64 linux/arm \
                   darwin/amd64 darwin/arm64 \
                   windows/amd64 windows/arm64

.PHONY: all build test vet tidy fmt clean dist release help

all: build

build:
	CGO_ENABLED=$(CGO_ENABLED) $(GO) -C $(GO_PKG_DIR) build -trimpath -ldflags "$(LDFLAGS)" -o $(PKG_ROOT)/$(BINARY)$(EXE) .

test:
	$(GO) -C $(GO_PKG_DIR) test ./...

vet:
	$(GO) -C $(GO_PKG_DIR) vet ./...

tidy:
	$(GO) -C $(GO_PKG_DIR) mod tidy

fmt:
	$(GO) -C $(GO_PKG_DIR) fmt ./...

clean:
	rm -f $(PKG_ROOT)/$(BINARY) $(GO_PKG_DIR)/$(BINARY) $(PKG_ROOT)/$(BINARY).exe
	rm -rf $(BUILD_DIR)

dist: dist-tar dist-checksums

dist-tar: build
	rm -rf $(STAGE_DIR)
	mkdir -p $(STAGE_DIR)
	install -m 0755 $(PKG_ROOT)/$(BINARY)$(EXE) $(STAGE_DIR)/$(BINARY)$(EXE)
	cp $(PKG_ROOT)/README.md $(STAGE_DIR)/README.md
	cp $(GO_PKG_DIR)/README.md $(STAGE_DIR)/GO.md
	cp $(PKG_ROOT)/doc/openapi.yaml $(STAGE_DIR)/openapi.yaml
	mkdir -p $(STAGE_DIR)/doc
	cp -R $(PKG_ROOT)/doc/example $(STAGE_DIR)/doc/
	tar -C $(BUILD_DIR) -czf $(BUILD_DIR)/$(PKG_NAME).tar.gz $(PKG_NAME)
	rm -rf $(STAGE_DIR)

dist-zip: build
	rm -rf $(STAGE_DIR)
	mkdir -p $(STAGE_DIR)
	install -m 0755 $(PKG_ROOT)/$(BINARY)$(EXE) $(STAGE_DIR)/$(BINARY)$(EXE)
	cp $(PKG_ROOT)/README.md $(STAGE_DIR)/README.md
	cp $(GO_PKG_DIR)/README.md $(STAGE_DIR)/GO.md
	cp $(PKG_ROOT)/doc/openapi.yaml $(STAGE_DIR)/openapi.yaml
	mkdir -p $(STAGE_DIR)/doc
	cp -R $(PKG_ROOT)/doc/example $(STAGE_DIR)/doc/
	(cd $(BUILD_DIR) && zip -qr $(PKG_NAME).zip $(PKG_NAME))
	rm -rf $(STAGE_DIR)

dist-checksums: dist-tar
	cd $(BUILD_DIR) && sha256sum $(PKG_NAME).tar.gz > $(PKG_NAME).tar.gz.sha256

release:
	@$(foreach p,$(RELEASE_TARGETS), \
		$(MAKE) dist-tar dist-checksums GOOS=$(word 1,$(subst /, ,$(p))) GOARCH=$(word 2,$(subst /, ,$(p))) VERSION=$(VERSION) || exit 1; )

help:
	@echo "Targets:"
	@echo "  all         - build the binary (default)"
	@echo "  build       - compile a static binary to $(BINARY)"
	@echo "  test        - run go test"
	@echo "  vet         - run go vet"
	@echo "  fmt         - run go fmt"
	@echo "  tidy        - go mod tidy"
	@echo "  clean       - remove build artifacts and dist/"
	@echo "  dist        - build a tar.gz release and write checksums"
	@echo "  dist-tar    - build a tar.gz release only"
	@echo "  dist-zip    - build a zip release"
	@echo "  dist-checksums - regenerate sha256 sums"
	@echo "  release     - build tar.gz for: $(RELEASE_TARGETS)"
	@echo "  help        - this help"
	@echo ""
	@echo "Variables (override on the command line):"
	@echo "  GOOS=$(GOOS)  GOARCH=$(GOARCH)  CGO_ENABLED=$(CGO_ENABLED)"
	@echo "  VERSION is taken from 'git describe' (fallback: 0.1.0-dev)"
