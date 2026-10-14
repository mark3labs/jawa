#!/bin/sh
# Prints the pinned shadcn-templ runtime that must run before the app bundle.
# Shared by build.sh (production) and dev.mjs (watch) so both stay consistent.
# Floating UI ships UMD: callers prepend this verbatim (never bundle it) so
# esbuild does not rewrite its browser globals into CommonJS or resolve npm requires.
set -eu
SHADCN="$(go list -m -f '{{.Dir}}' github.com/axadrn/shadcn-templ/v2)"
for module in baseui/lifecycle floatingui/floating_ui_core floatingui/floating_ui_dom baseui/direction baseui/composite baseui/tabbable baseui/portal baseui/mark_others baseui/floating_focus_manager baseui/internal_backdrop baseui/scroll_lock baseui/use_anchor_positioning baseui/use_dismiss baseui/use_list_navigation baseui/use_typeahead baseui/use_transition_status select/select checkbox/checkbox switch/switch; do
  cat "$SHADCN/components/$module.js"
  printf '\n'
done
