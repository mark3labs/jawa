#!/bin/sh
# Builds the embedded frontend: JS bundle (Datastar + Rocket + SortableJS),
# compiled Tailwind/Nova CSS and the self-hosted Inter font.
set -eu
cd "$(dirname "$0")"
SHADCN="$(go list -m -f '{{.Dir}}' github.com/axadrn/shadcn-templ/v2)"
# Bundle only the pinned shadcn behavior we use, before the Rocket owners.
# Keep entries temporary: component sources remain owned by the Go module.
# Floating UI ships UMD: concatenate before minifying (not bundling) so esbuild
# does not rewrite its browser globals into CommonJS or resolve npm requires.
trap 'rm -f src/.build.js src/.build.css src/.runtime.js' EXIT
: > src/.build.js
: > src/.runtime.js
for module in baseui/lifecycle floatingui/floating_ui_core floatingui/floating_ui_dom baseui/direction baseui/composite baseui/tabbable baseui/portal baseui/mark_others baseui/floating_focus_manager baseui/internal_backdrop baseui/scroll_lock baseui/use_anchor_positioning baseui/use_dismiss baseui/use_list_navigation baseui/use_typeahead baseui/use_transition_status select/select checkbox/checkbox switch/switch; do
  cat "$SHADCN/components/$module.js" >> src/.runtime.js
  printf '\n' >> src/.runtime.js
done
printf 'import "./select.js";\nimport "./app.js";\n' >> src/.build.js
esbuild src/.build.js --bundle --minify --outfile=../assets/app.js
cat src/.runtime.js ../assets/app.js > src/.build.js
esbuild src/.build.js --minify --outfile=../assets/app.js
cp node_modules/@fontsource-variable/inter/files/inter-latin-wght-normal.woff2 ../assets/inter.woff2
# Embed redistribution notices alongside the assets in the standalone binary.
cp DATASTAR-LICENSE.md ../assets/DATASTAR-LICENSE.txt
cp INTER-LICENSE ../assets/INTER-LICENSE.txt
cp src/style.css src/.build.css
printf '\n@source "%s/components";\n' "$SHADCN" >> src/.build.css
tailwindcss -i src/.build.css -o ../assets/style.css --minify
node build-pwa.mjs
