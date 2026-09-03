#!/usr/bin/env bash
#
# Install ferrite from a directory of built binaries — an unpacked release
# tarball, or the checkout after `make build`. Both have the same five
# programs in them, so both install the same way.
#
# The layout it installs into exists to answer one question: what may be
# deleted without taking the television down? Everything under lib/ is
# replaceable and belongs to a release; everything under config/ and share/
# is the box's own and is never overwritten. Nothing lives in a build tree.
# The daemon used to spawn `./target/release/dvb-rs` with the checkout as its
# working directory, which made `cargo clean` — a documented, ordinary thing
# to run — the command that stops the box from being able to tune. It took
# three days for anyone to notice.
#
#   ~/.local/lib/ferrite/     isdbd, dvb-rs, b25-rs, arib-caption
#   ~/.local/bin/ferrite-tui  the remote control, on $PATH
#   ~/.config/ferrite/        isdbd.toml, channels.json   (yours; never clobbered)
#   ~/.local/share/ferrite/   isdbd.db, recordings/       (yours; never touched)
#
# --system installs the same shape under /usr/local/lib/ferrite, /etc/ferrite
# and /var/lib/ferrite instead, for a box where the daemon is not run by the
# user sitting at it. Note that a *user* unit is what this project wants on a
# single-tuner box: the DVB device and the B-CAS reader are reached through
# the invoking user's permissions, and pcscd's polkit rule is per-user.
set -euo pipefail

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# In a release tarball this script sits beside the binaries; in the checkout
# it is in scripts/ and they are one level up (and in cargo's target/).
[[ -f "$SRC/isdbd" ]] || SRC="$(dirname "$SRC")"

SYSTEM=0
RESTART=1
for arg in "$@"; do
  case "$arg" in
    --system)    SYSTEM=1 ;;
    --no-restart) RESTART=0 ;;
    -h|--help)
      sed -n '2,28p' "${BASH_SOURCE[0]}" | sed 's/^#\s\?//'
      exit 0 ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done

if [[ $SYSTEM -eq 1 ]]; then
  LIBDIR=/usr/local/lib/ferrite
  BINDIR=/usr/local/bin
  CFGDIR=/etc/ferrite
  DATADIR=/var/lib/ferrite
  UNITDIR=/etc/systemd/system
  SUDO=sudo
  SYSTEMCTL="sudo systemctl"
else
  LIBDIR="$HOME/.local/lib/ferrite"
  BINDIR="$HOME/.local/bin"
  CFGDIR="$HOME/.config/ferrite"
  DATADIR="$HOME/.local/share/ferrite"
  UNITDIR="$HOME/.config/systemd/user"
  SUDO=""
  SYSTEMCTL="systemctl --user"
fi

say() { printf '  %s\n' "$*"; }

# ── the programs ──────────────────────────────────────────────────
#
# Found in either shape: flat (a release tarball) or in the build trees the
# Makefile writes to (the checkout).
find_bin() {
  local name="$1" p
  for p in "$SRC/$name" \
           "$SRC/target/release/$name" \
           "$SRC/libaribb25-rs/target/release/$name"; do
    # -f as well as -x: `dvb-rs` is also the name of a *submodule
    # directory* in the checkout, and a directory is executable. Without
    # this the search finds the source tree and `install` refuses it with
    # "omitting directory", one program into the run.
    [[ -f "$p" && -x "$p" ]] && { printf '%s' "$p"; return 0; }
  done
  return 1
}

echo "=== ferrite install ==="
say "from    $SRC"
say "into    $LIBDIR"

$SUDO mkdir -p "$LIBDIR" "$BINDIR" "$CFGDIR" "$DATADIR" "$UNITDIR"

missing=()
for name in isdbd dvb-rs b25-rs arib-caption ferrite-tui; do
  if src="$(find_bin "$name")"; then
    # Install to a temporary name and rename, so a running daemon is
    # replaced atomically rather than being written through under its feet
    # (ETXTBSY on a binary systemd is about to restart).
    dest="$LIBDIR/$name"
    $SUDO install -m 0755 "$src" "$dest.new"
    $SUDO mv -f "$dest.new" "$dest"
    say "installed $name  ($(du -h "$src" | cut -f1))"
  else
    missing+=("$name")
  fi
done
if [[ ${#missing[@]} -gt 0 ]]; then
  echo "ERROR: not built: ${missing[*]}" >&2
  echo "       run 'make build' in the checkout, or use a complete release tarball" >&2
  exit 1
fi

# The TUI runs on whatever machine you are sitting at, so it goes on $PATH.
# The link points inside the install prefix, never back into a checkout —
# `make install-service` used to symlink $HOME/.local/bin/ferrite-tui at the
# source tree, which is the same dependency on a build directory that this
# layout exists to remove.
$SUDO ln -sfn "$LIBDIR/ferrite-tui" "$BINDIR/ferrite-tui"
say "linked    $BINDIR/ferrite-tui"

# ── the config ────────────────────────────────────────────────────
#
# Never overwritten. A box's isdbd.toml names its own hardware (which
# encoder, which adapter, which frequencies) and its channels.json is
# curated by hand and untracked — replacing either from a release is how an
# upgrade silently unconfigures a working television.
if [[ ! -f "$CFGDIR/isdbd.toml" ]]; then
  example="$SRC/configs/isdbd.example.toml"
  [[ -f "$example" ]] || example="$SRC/isdbd.example.toml"
  if [[ -f "$example" ]]; then
    # The example is written in the --system paths, TOML having no ~ to
    # write the other form with. For a user install, point them at this
    # prefix — a fresh config that names /usr/local is a fresh config that
    # cannot spawn anything, which is the failure this whole layout exists
    # to stop shipping.
    if [[ $SYSTEM -eq 1 ]]; then
      $SUDO install -m 0644 "$example" "$CFGDIR/isdbd.toml"
    else
      sed -e "s|/usr/local/lib/ferrite|$LIBDIR|g" \
          -e "s|/etc/ferrite|$CFGDIR|g" \
          -e "s|/var/lib/ferrite|$DATADIR|g" \
          "$example" > "$CFGDIR/isdbd.toml"
      chmod 0644 "$CFGDIR/isdbd.toml"
    fi
    say "wrote     $CFGDIR/isdbd.toml (from the example — check it before starting)"
  fi
else
  say "kept      $CFGDIR/isdbd.toml (yours)"
fi

# ── the unit ──────────────────────────────────────────────────────
unit="$SRC/scripts/ferrite.service"
[[ -f "$unit" ]] || unit="$SRC/ferrite.service"
if [[ $SYSTEM -eq 1 ]]; then
  unit="$SRC/scripts/isdbd.service"
  [[ -f "$unit" ]] || unit="$SRC/isdbd.service"
fi
if [[ -f "$unit" ]]; then
  $SUDO install -m 0644 "$unit" "$UNITDIR/ferrite.service"
  say "installed $UNITDIR/ferrite.service"
  $SYSTEMCTL daemon-reload
fi

echo
echo "=== installed ==="
"$LIBDIR/isdbd" --version 2>/dev/null || true
say "config    $CFGDIR/isdbd.toml"
say "data      $DATADIR"

if [[ $RESTART -eq 1 ]] && $SYSTEMCTL is-enabled ferrite.service >/dev/null 2>&1; then
  $SYSTEMCTL restart ferrite.service
  sleep 2
  $SYSTEMCTL --no-pager --lines=0 status ferrite.service | head -4
else
  echo
  echo "to start it:  $SYSTEMCTL enable --now ferrite.service"
fi
