#!/bin/sh
# Builds the embedded frontend: JS bundle (Datastar + SortableJS),
# compiled Tailwind/Nova CSS and the self-hosted Inter font.
set -eu
cd "$(dirname "$0")"
SHADCN="$(go list -m -f '{{.Dir}}' github.com/axadrn/shadcn-templ/v2)"
# Bundle only the pinned shadcn behavior we use, before the Datastar scan.
# Keep entries temporary: component sources remain owned by the Go module.
# The runtime list lives in runtime.sh, shared with the dev watch (dev.mjs).
trap 'rm -f src/.build.js src/.build.css src/.runtime.js' EXIT
sh ./runtime.sh > src/.runtime.js
printf '\nimport "./app.js";\nimport "./datastar.js";\n' > src/.build.js
esbuild src/.build.js --bundle --minify --outfile=../assets/app.js
cat src/.runtime.js ../assets/app.js > src/.build.js
esbuild src/.build.js --minify --outfile=../assets/app.js
cp node_modules/@fontsource-variable/inter/files/inter-latin-wght-normal.woff2 ../assets/inter.woff2
# Embed redistribution notices alongside the assets in the standalone binary.
cp DATASTAR-LICENSE.md ../assets/DATASTAR-LICENSE.txt
cp INTER-LICENSE ../assets/INTER-LICENSE.txt
cp MERMAID-LICENSE ../assets/MERMAID-LICENSE.txt
cp src/style.css src/.build.css
printf '\n@source "%s/components";\n' "$SHADCN" >> src/.build.css
tailwindcss -i src/.build.css -o ../assets/style.css --minify
node build-pwa.mjs
