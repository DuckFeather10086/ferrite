import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  output: "export",
  // Build into the default out/; a postbuild step copies to the Go embed dir.
  trailingSlash: false,
  // A constant, because this build output is committed: `internal/web/dist` is
  // what `go:embed` puts in the binary, and it is what the release workflow
  // ships (it never runs a web build of its own). Next's default id is fresh
  // per build, and it appears both in the payload paths under
  // `_next/static/<id>/` and inside every RSC payload — so rebuilding the UI
  // without changing a line of it rewrote 43 committed files, which buries a
  // real diff and makes `git status` lie about what changed.
  //
  // Safe to pin here specifically because nothing caches these: the daemon
  // serves the whole bundle `no-store`, the one exception being the caption
  // font, which is versioned by filename instead. The chunk filenames are
  // content-hashed by Next either way, so a real change still busts them.
  generateBuildId: () => "ferrite",
};

export default nextConfig;
