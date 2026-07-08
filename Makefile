.PHONY: all deps build run clean web rust go

# ── top-level targets ─────────────────────────────────────────────

all: build

# One-shot: init everything then build.
deps:
	@echo "=== git submodules ==="
	git submodule update --init --recursive
	@echo "=== bun install ==="
	cd web && bun install --frozen-lockfile 2>/dev/null || bun install
	@echo "=== go mod tidy ==="
	go mod tidy
	@echo "=== deps done ==="

# Build everything: Rust binaries, web UI, Go daemon.
build: rust web go
	@echo "=== build complete ==="
	@ls -lh target/release/dvbr libaribb25-rs/target/release/b25-rs internal/web/dist/index.html isdbd 2>/dev/null | awk '{print $$NF, $$5}'

# Run from the ferrite/ directory.
run: build
	@echo "=== starting isdbd ==="
	./isdbd --config configs/isdbd.toml

# ── component-by-component ────────────────────────────────────────

rust:
	@echo "=== cargo build (release) ==="
	cargo build --release -p dvbr -p b25-rs
	@test -x target/release/dvbr || (echo "ERROR: target/release/dvbr not found" && exit 1)
	@test -x libaribb25-rs/target/release/b25-rs || (echo "ERROR: b25-rs not found" && exit 1)

web:
	@echo "=== next.js static export ==="
	cd web && bun run build
	@test -f internal/web/dist/index.html || (echo "ERROR: web build failed" && exit 1)

go:
	@echo "=== go build ==="
	@export PATH="$${GOROOT:-/usr/local/go}/bin:$$PATH" && go build -o isdbd ./cmd/isdbd/
	@test -x isdbd || (echo "ERROR: go build failed" && exit 1)

# ── clean ─────────────────────────────────────────────────────────

clean:
	rm -rf isdbd internal/web/dist
	cargo clean
	cd web && rm -rf out .next
	@echo "=== clean done ==="
