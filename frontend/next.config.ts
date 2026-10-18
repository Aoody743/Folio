import type { NextConfig } from "next";
const config: NextConfig = {
  output: "export",
  trailingSlash: true,
  generateBuildId: async () => process.env.NEXT_BUILD_ID || "folio-development",
  images: { unoptimized: true },
  poweredByHeader: false,
};
export default config;
