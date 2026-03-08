# AngelLab — Makefile
# Three binaries: labd (daemon), lab (CLI), angel (worker)

BUILD_DIR  := build
LABD_BIN   := $(BUILD_DIR)/labd
LAB_BIN    := $(BUILD_DIR)/lab
ANGEL_BIN  := $(BUILD_DIR)/angel

GO      := go
GOFMT   := gofmt

# CGO is required for go-sqlite3.
export CGO_ENABLED=1

# Build-time metadata injected via ldflags.
VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILDTIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
COMMIT    := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

LDFLAGS := -ldflags "-s -w \
	-X github.com/nacreousdawn596/angellab/pkg/version.Version=$(VERSION) \
	-X github.com/nacreousdawn596/angellab/pkg/version.BuildTime=$(BUILDTIME) \
	-X github.com/nacreousdawn596/angellab/pkg/version.Commit=$(COMMIT)"

GOFLAGS := -trimpath

INSTALL_DIR  := /usr/local/bin
SYSTEMD_DIR  := /etc/systemd/system
CONF_DIR     := /etc/angellab

.PHONY: all build labd lab angel install uninstall \
        test test-unit test-integration test-stress test-race test-bench \
        lint fmt vet tidy clean help

## all: build all three binaries (alias for build)
all: build

## build: compile labd, lab, and angel into build/
build: $(LABD_BIN) $(LAB_BIN) $(ANGEL_BIN)

$(BUILD_DIR):
	@mkdir -p $(BUILD_DIR)

$(LABD_BIN): $(BUILD_DIR) $(shell find cmd/labd internal pkg -name '*.go' 2>/dev/null)
	@echo "  BUILD   labd"
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(LABD_BIN) ./cmd/labd

$(LAB_BIN): $(BUILD_DIR) $(shell find cmd/lab pkg/ipc pkg/version -name '*.go' 2>/dev/null)
	@echo "  BUILD   lab"
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(LAB_BIN) ./cmd/lab

$(ANGEL_BIN): $(BUILD_DIR) $(shell find cmd/angel internal/angels pkg -name '*.go' 2>/dev/null)
	@echo "  BUILD   angel"
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(ANGEL_BIN) ./cmd/angel

## labd: build only the daemon
labd: $(LABD_BIN)

## lab: build only the CLI client
lab: $(LAB_BIN)

## angel: build only the angel worker binary
angel: $(ANGEL_BIN)

## install: install all binaries, systemd unit, and default config
install: build
	@echo "  INSTALL binaries"
	install -Dm755 $(LABD_BIN)  $(INSTALL_DIR)/labd
	install -Dm755 $(LAB_BIN)   $(INSTALL_DIR)/lab
	install -Dm755 $(ANGEL_BIN) $(INSTALL_DIR)/angel
	@echo "  INSTALL systemd unit"
	install -Dm644 scripts/angellab.service $(SYSTEMD_DIR)/angellab.service
	@echo "  INSTALL config (if not present)"
	@[ -f $(CONF_DIR)/angellab.toml ] || install -Dm644 configs/angellab.toml $(CONF_DIR)/angellab.toml
	@echo "  RUN     scripts/install.sh"
	bash scripts/install.sh
	@echo "  RELOAD  systemd"
	systemctl daemon-reload
	@echo ""
	@echo "Installed. Start with:"
	@echo "  sudo systemctl start angellab"
	@echo "  lab status"

## uninstall: remove installed files (preserves data in /var/lib/angellab)
uninstall:
	systemctl stop angellab 2>/dev/null || true
	systemctl disable angellab 2>/dev/null || true
	rm -f $(INSTALL_DIR)/labd $(INSTALL_DIR)/lab $(INSTALL_DIR)/angel
	rm -f $(SYSTEMD_DIR)/angellab.service
	@echo "Binaries and unit removed. Data in /var/lib/angellab preserved."

## test: run unit + integration tests with race detector
test: test-unit test-integration

## test-unit: run unit tests only (pkg + internal packages)
test-unit:
	@echo "  TEST    unit"
	$(GO) test -v -race -count=1 ./pkg/... ./internal/...

## test-integration: run integration tests (no root required)
test-integration:
	@echo "  TEST    integration"
	$(GO) test -v -race -count=1 -timeout=120s ./test/integration/...

## test-stress: run Sentinel latency and stress tests
test-stress:
	@echo "  TEST    stress"
	$(GO) test -v -count=1 -timeout=120s ./test/stress/...

## test-bench: run Sentinel pipeline benchmarks with memory stats
test-bench:
	@echo "  BENCH   sentinel"
	$(GO) test -bench=. -benchmem -benchtime=5s ./test/stress/...

## test-race: run all tests with race detector (full suite)
test-race:
	@echo "  TEST    race"
	$(GO) test -race -count=1 -timeout=180s ./...

## lint: run golangci-lint (install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)
lint:
	golangci-lint run ./...

## fmt: format all Go source files
fmt:
	$(GO) fmt ./...

## vet: run go vet on all packages
vet:
	$(GO) vet ./...

## tidy: tidy go.mod and go.sum
tidy:
	$(GO) mod tidy

## clean: remove build artifacts
clean:
	rm -rf $(BUILD_DIR)

## help: show available make targets
help:
	@echo ""
	@echo "AngelLab build targets:"
	@grep -E '^## ' Makefile | sed 's/^## /  /'
	@echo ""
