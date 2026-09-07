import { vitePlugin as remix } from "@remix-run/dev";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";
import { resolve } from "path";

export default defineConfig({
  plugins: [
    tailwindcss(),
    remix({
      ssr: true,
    }),
  ],
  resolve: {
    alias: {
      "~": resolve(__dirname, "./app"),
    },
  },
  optimizeDeps: {
    exclude: ["@remix-run/react"],
    include: [
      "@datum-cloud/datum-ui/badge",
      "@datum-cloud/datum-ui/breadcrumb",
      "@datum-cloud/datum-ui/button",
      "@datum-cloud/datum-ui/card",
      "@datum-cloud/datum-ui/empty-content",
      "@datum-cloud/datum-ui/input",
      "@datum-cloud/datum-ui/page-title",
      "@datum-cloud/datum-ui/sidebar",
      "@datum-cloud/datum-ui/table",
      "@datum-cloud/datum-ui/toast",
      "lucide-react",
      "js-yaml",
    ],
  },
  server: {
    host: "0.0.0.0",
    port: 3000,
  },
});
