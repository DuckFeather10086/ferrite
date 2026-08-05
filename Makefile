.PHONY: all deps build run clean web rust go \
        install-service uninstall-service restart status logs

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
	cargo build --release -p dvbr -p b25-rs -p arib-caption
	@test -x target/release/dvbr || (echo "ERROR: target/release/dvbr not found" && exit 1)
	@test -x libaribb25-rs/target/release/b25-rs || (echo "ERROR: b25-rs not found" && exit 1)
	@test -x target/release/arib-caption || (echo "ERROR: arib-caption not found" && exit 1)

web:
	@echo "=== next.js static export ==="
	cd web && bun run build
	@test -f internal/web/dist/index.html || (echo "ERROR: web build failed" && exit 1)

go:
	@echo "=== go build ==="
	@export PATH="$${GOROOT:-/usr/local/go}/bin:$$PATH" && go build -o isdbd ./cmd/isdbd/
	@test -x isdbd || (echo "ERROR: go build failed" && exit 1)
	@export PATH="$${GOROOT:-/usr/local/go}/bin:$$PATH" && go build -o ferrite-tui ./cmd/ferrite-tui/
	@test -x ferrite-tui || (echo "ERROR: tui build failed" && exit 1)

# ── run it as a service ───────────────────────────────────────────
#
# A systemd *user* service, because the tuner and the B-CAS reader are
# reached with this user's permissions. `loginctl enable-linger $(USER)`
# (already on here) is what makes it start at boot instead of at login.
#
# ferrite-tui is symlinked rather than copied, so `make go` is enough to
# update it.

# Depends on `go`, not `build`: installing a service should not rebuild the
# web UI or reach for cargo. The Rust binaries the daemon spawns must
# already be there, so check rather than build them.
install-service: go
	@test -x target/release/dvbr || (echo "ERROR: target/release/dvbr missing — run 'make rust'" && exit 1)
	@test -x libaribb25-rs/target/release/b25-rs || (echo "ERROR: b25-rs missing — run 'make rust'" && exit 1)
	@test -x target/release/arib-caption || (echo "ERROR: arib-caption missing — run 'make rust'" && exit 1)
	@mkdir -p $(HOME)/.config/systemd/user $(HOME)/.local/bin
	sed 's|@DIR@|$(CURDIR)|g' scripts/ferrite.service \
		> $(HOME)/.config/systemd/user/ferrite.service
	ln -sf $(CURDIR)/ferrite-tui $(HOME)/.local/bin/ferrite-tui
	systemctl --user daemon-reload
	systemctl --user enable --now ferrite.service
	@sleep 2
	@systemctl --user --no-pager --lines=0 status ferrite.service | head -4
	@echo '=== ferrite is up; run `ferrite-tui` from anywhere ==='

uninstall-service:
	-systemctl --user disable --now ferrite.service
	rm -f $(HOME)/.config/systemd/user/ferrite.service $(HOME)/.local/bin/ferrite-tui
	systemctl --user daemon-reload

# After `make go`: pick up the new binary without a full reinstall.
restart: go
	systemctl --user restart ferrite.service
	@sleep 2
	@systemctl --user --no-pager --lines=0 status ferrite.service | head -4

status:
	@systemctl --user --no-pager --lines=0 status ferrite.service || true
	@curl -s --max-time 3 localhost:8010/api/status || echo '(daemon not answering on :8010)'

logs:
	journalctl --user -u ferrite.service -f

# ── clean ─────────────────────────────────────────────────────────

clean:
	rm -rf isdbd internal/web/dist
	cargo clean
	cd web && rm -rf out .next
	@echo "=== clean done ==="
