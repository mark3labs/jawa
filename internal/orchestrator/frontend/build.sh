#!/bin/sh
# Builds the embedded frontend: JS bundle (Datastar + Rocket + SortableJS),
# compiled Tailwind/Nova CSS and the self-hosted Inter font.
set -eu
cd "$(dirname "$0")"
SHADCN="$(go list -m -f '{{.Dir}}' github.com/axadrn/shadcn-templ/v2)"
esbuild src/app.js --bundle --minify --outfile=../assets/app.js
cp node_modules/@fontsource-variable/inter/files/inter-latin-wght-normal.woff2 ../assets/inter.woff2
# Embed redistribution notices alongside the assets in the standalone binary.
cp DATASTAR-LICENSE.md ../assets/DATASTAR-LICENSE.txt
cp INTER-LICENSE ../assets/INTER-LICENSE.txt
cp src/style.css src/.build.css
trap 'rm -f src/.build.css' EXIT
printf '\n@source "%s/components";\n' "$SHADCN" >> src/.build.css
tailwindcss -i src/.build.css -o ../assets/style.css --minify
