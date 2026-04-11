BINARY    := gortex
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)

.PHONY: build build-onnx build-gomlx build-hugot \
       test bench bench-rpi bench-rpi-quick bench-rpi-profile bench-compare \
       lint fmt clean install \
       deps-onnx deps-gomlx deps-hugot deps-vectors

# ---------------------------------------------------------------------------
# Build variants
# ---------------------------------------------------------------------------

build:
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/gortex/

build-onnx: deps-onnx
	go build -tags embeddings_onnx -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/gortex/

build-gomlx: deps-gomlx
	go build -tags embeddings_gomlx -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/gortex/

build-hugot: deps-hugot
	go build -tags embeddings_hugot -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/gortex/

test:
	go test -race ./...

bench:
	go test -bench=. -benchmem -count=1 -benchtime=1s \
		./internal/parser/languages/ \
		./internal/graph/ \
		./internal/search/ \
		./internal/resolver/ \
		./internal/query/ \
		./internal/indexer/ \
		./internal/analysis/

# RPi / low-resource device benchmarks
BENCH_BASELINE ?= results/bench-baseline.txt

bench-rpi:
	./scripts/bench-rpi.sh

bench-rpi-quick:
	./scripts/bench-rpi.sh --quick

bench-rpi-profile:
	./scripts/bench-rpi.sh --profile

bench-compare:
	./scripts/bench-rpi.sh --compare $(BENCH_BASELINE)

bench-save-baseline:
	./scripts/bench-rpi.sh
	@cp $$(ls -t results/bench-*.txt | head -1) $(BENCH_BASELINE)
	@echo "✓ Baseline saved to $(BENCH_BASELINE)"

lint:
	golangci-lint run --timeout=5m

fmt:
	gofmt -s -w .

clean:
	rm -f $(BINARY) gortex-linux gortex-rpi

install:
	go install -ldflags '$(LDFLAGS)' ./cmd/gortex/

# Cross-compile for Raspberry Pi (ARM64)
build-rpi:
	CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
		go build -ldflags '$(LDFLAGS)' -o gortex-rpi ./cmd/gortex/
	@echo "✓ Built gortex-rpi (linux/arm64)"

# Cross-compile for Raspberry Pi (ARMv7 / 32-bit)
build-rpi32:
	CGO_ENABLED=1 GOOS=linux GOARCH=arm GOARM=7 CC=arm-linux-gnueabihf-gcc \
		go build -ldflags '$(LDFLAGS)' -o gortex-rpi32 ./cmd/gortex/
	@echo "✓ Built gortex-rpi32 (linux/arm/v7)"

# ---------------------------------------------------------------------------
# Web UI (Next.js)
# ---------------------------------------------------------------------------

.PHONY: install-ui dev-ui build-ui

install-ui:
	cd web && npm install

dev-ui:
	cd web && npm run dev

build-ui:
	cd web && npm run build

# ---------------------------------------------------------------------------
# Embedding backend dependencies
# ---------------------------------------------------------------------------

# ONNX Runtime — system library required for -tags embeddings_onnx
deps-onnx:
	@echo "=== ONNX Runtime dependency ==="
ifeq ($(shell uname -s),Darwin)
	@command -v brew >/dev/null 2>&1 || { echo "Error: Homebrew required. Install from https://brew.sh"; exit 1; }
	@brew list onnxruntime >/dev/null 2>&1 || brew install onnxruntime
	@echo "✓ onnxruntime installed (macOS/Homebrew)"
else ifeq ($(shell uname -s),Linux)
	@dpkg -s libonnxruntime-dev >/dev/null 2>&1 || { echo "Run: sudo apt install libonnxruntime-dev"; exit 1; }
	@echo "✓ libonnxruntime-dev installed (Linux/apt)"
endif
	go get github.com/yalue/onnxruntime_go@latest
	@echo "✓ Go ONNX bindings ready"

# GoMLX — XLA/PJRT plugin auto-downloads on first run (~100MB)
deps-gomlx:
	@echo "=== GoMLX dependency ==="
	go get github.com/gomlx/gomlx@latest
	go get github.com/gomlx/onnx-gomlx@latest
	@echo "✓ GoMLX + ONNX converter installed"
	@echo "  Note: XLA/PJRT plugin will auto-download on first run (~100MB)"

# Hugot — uses same XLA/PJRT backend as GoMLX
deps-hugot:
	@echo "=== Hugot dependency ==="
	go get github.com/knights-analytics/hugot@latest
	@echo "✓ Hugot installed"
	@echo "  Note: XLA/PJRT plugin will auto-download on first run (~100MB)"

# Prepare GloVe word vectors for built-in static embeddings
deps-vectors:
	@echo "=== Preparing GloVe word vectors ==="
	@test -f internal/embedding/data/vectors.bin.gz && echo "✓ Vectors already prepared" || bash scripts/prepare_vectors.sh

# ---------------------------------------------------------------------------
# Eval framework
# ---------------------------------------------------------------------------

EVAL_DIR     := eval
EVAL_VENV    := $(EVAL_DIR)/.venv
EVAL_PYTHON  := $(EVAL_VENV)/bin/python
EVAL_PIP     := $(EVAL_VENV)/bin/pip
EVAL_CLI     := $(EVAL_VENV)/bin/gortex-eval
EVAL_ANALYZE := $(EVAL_VENV)/bin/gortex-eval-analyze

MODEL  ?= claude-sonnet
MODE   ?= baseline
SLICE  ?= 0:5
SUBSET ?= lite

.PHONY: eval-setup eval-test eval-test-all eval-list \
        eval-single eval-matrix eval-debug eval-summary eval-compare eval-tools

# Setup: create venv and install deps
eval-setup: build
	@test -d $(EVAL_VENV) || python3 -m venv $(EVAL_VENV)
	$(EVAL_PIP) install -q -e "$(EVAL_DIR)[dev]"
	@echo "✓ Eval framework ready. Binary: ./$(BINARY)"

# Build linux/amd64 binary for container injection (requires podman/docker)
eval-build-linux:
	podman run --rm --platform linux/amd64 -v $(CURDIR):/src -w /src golang:1.25 \
		bash -c "apt-get update -qq && apt-get install -y -qq libtree-sitter-dev && go build -ldflags '$(LDFLAGS)' -o gortex-linux ./cmd/gortex/"
	@echo "✓ Built gortex-linux (linux/amd64)"

# Run Python eval tests
eval-test:
	$(EVAL_PYTHON) -m pytest $(EVAL_DIR)/tests/ -q

# Run all tests (Go + Python)
eval-test-all: test eval-test

# List available configs
eval-list: eval-setup
	$(EVAL_CLI) list-configs

# Single (model, mode) run
eval-single: eval-setup
	$(EVAL_CLI) single -m $(MODEL) --mode $(MODE) --subset $(SUBSET) --slice $(SLICE)

# Full A/B matrix
eval-matrix: eval-setup
	$(EVAL_CLI) matrix --models claude-sonnet claude-haiku \
		--modes baseline native native_augment \
		--subset $(SUBSET) --slice $(SLICE)

# Debug a single instance
eval-debug: eval-setup
	$(EVAL_CLI) debug -m $(MODEL) --mode $(MODE) -i $(INSTANCE)

# Analyze results
eval-summary:
	$(EVAL_ANALYZE) summary results/

eval-compare:
	$(EVAL_ANALYZE) compare-modes results/ -m $(MODEL)

eval-tools:
	$(EVAL_ANALYZE) tool-usage results/
