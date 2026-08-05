import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  output: "export",
  // Build into the default out/; a postbuild step copies to the Go embed dir.
  trailingSlash: false,
};

export default nextConfig;
