.PHONY: all deps build run clean web rust go \
        install install-service uninstall-service restart status logs

# What /api/status reports, and what the TUI prints under its wordmark.
# `cmd/isdbd` declares `var version = "dev"` for -ldflags to overwrite, and
# nothing was overwriting it, so every build called itself "dev" — including
# the one running as a service, which is the one you most need to identify.
# git describe answers all three questions at once: a tagged build says
# v0.1.0, a build past the tag says how far past and from which commit, and a
# build from an uncommitted tree says `-dirty`.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

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
	@ls -lh target/release/dvb-rs libaribb25-rs/target/release/b25-rs internal/web/dist/index.html isdbd 2>/dev/null | awk '{print $$NF, $$5}'

# Run from the ferrite/ directory.
run: build
	@echo "=== starting isdbd ==="
	./isdbd --config configs/isdbd.toml

# ── component-by-component ────────────────────────────────────────

rust:
	@echo "=== cargo build (release) ==="
	cargo build --release -p dvb-rs -p arib-caption
	@# b25-rs is a workspace of its own — Cargo.toml *excludes* libaribb25-rs,
	@# because it vendors the aribb25 C bindings and building it needs its own
	@# feature resolution. So it cannot be named as a -p of the outer build,
	@# which is what this target used to try; it has to be built from in there,
	@# and that is also where its target/ (and the path the config names) is.
	cd libaribb25-rs && cargo build --release -p b25-rs
	@test -x target/release/dvb-rs || (echo "ERROR: target/release/dvb-rs not found" && exit 1)
	@test -x libaribb25-rs/target/release/b25-rs || (echo "ERROR: b25-rs not found" && exit 1)
	@test -x target/release/arib-caption || (echo "ERROR: arib-caption not found" && exit 1)

web:
	@echo "=== next.js static export ==="
	cd web && bun run build
	@test -f internal/web/dist/index.html || (echo "ERROR: web build failed" && exit 1)

go:
	@echo "=== go build ==="
	@export PATH="$${GOROOT:-/usr/local/go}/bin:$$PATH" && go build -ldflags "-X main.version=$(VERSION)" -o isdbd ./cmd/isdbd/
	@test -x isdbd || (echo "ERROR: go build failed" && exit 1)
	@export PATH="$${GOROOT:-/usr/local/go}/bin:$$PATH" && go build -o ferrite-tui ./cmd/ferrite-tui/
	@test -x ferrite-tui || (echo "ERROR: tui build failed" && exit 1)

# ── install it ────────────────────────────────────────────────────
#
# A systemd *user* service, because the tuner and the B-CAS reader are
# reached with this user's permissions. `loginctl enable-linger $(USER)`
# (already on here) is what makes it start at boot instead of at login.
#
# The binaries are *copied* out of the build trees into ~/.local/lib/ferrite
# and the service runs from there. It used to run from the checkout, with
# `./target/release/dvb-rs` in its config and the checkout as its working
# directory — which made `cargo clean` (below, in this same Makefile) the
# command that stops the box tuning. It did, and nobody noticed for three
# days. scripts/install.sh has the layout and the reasoning.

install: build
	./scripts/install.sh

# Kept as the old name for muscle memory; the shape it installed is gone.
install-service: install

uninstall-service:
	-systemctl --user disable --now ferrite.service
	rm -f $(HOME)/.config/systemd/user/ferrite.service $(HOME)/.local/bin/ferrite-tui
	rm -rf $(HOME)/.local/lib/ferrite
	systemctl --user daemon-reload
	@echo "=== config and recordings kept: $(HOME)/.config/ferrite, $(HOME)/.local/share/ferrite ==="

# The day-to-day handle: rebuild the Go daemon, put it where the service
# runs from, restart. Depends on `go` and not `build` because this is the
# edit-compile-look loop and the Rust side rarely moves — but it does have
# to *install*, or the restart picks up the old binary and the change you
# just made appears not to work.
restart: go
	@install -m 0755 isdbd $(HOME)/.local/lib/ferrite/isdbd.new
	@mv -f $(HOME)/.local/lib/ferrite/isdbd.new $(HOME)/.local/lib/ferrite/isdbd
	@install -m 0755 ferrite-tui $(HOME)/.local/lib/ferrite/ferrite-tui.new
	@mv -f $(HOME)/.local/lib/ferrite/ferrite-tui.new $(HOME)/.local/lib/ferrite/ferrite-tui
	systemctl --user restart ferrite.service
	@sleep 2
	@systemctl --user --no-pager --lines=0 status ferrite.service | head -4

status:
	@systemctl --user --no-pager --lines=0 status ferrite.service || true
	@curl -s --max-time 3 localhost:8010/api/status || echo '(daemon not answering on :8010)'

logs:
	journalctl --user -u ferrite.service -f

# ── clean ─────────────────────────────────────────────────────────

# Safe to run now, which it was not before: the service runs from
# ~/.local/lib/ferrite and nothing it needs is under this directory. Before
# the install layout existed, this target removed the daemon's tuner.
clean:
	rm -rf isdbd ferrite-tui internal/web/dist
	cargo clean
	cd libaribb25-rs && cargo clean
	cd web && rm -rf out .next
	@echo "=== clean done (the installed service is untouched) ==="
