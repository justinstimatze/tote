#!/usr/bin/env bash
# Adds tote's Stop hook (tote/check.py) to a project's .claude/settings.local.json, which is gitignored.
# Merges: the file's other settings and hooks are kept, a backup is written first, and an existing tote hook is
# replaced rather than added twice.
#   install.sh                 this repo
#   install.sh PROJECT_DIR     another project; the hook still runs tote from this repo
#   TOTE_SKIP_IF=REGEX install.sh PROJECT_DIR   the audit skips any turn whose transcript window matches REGEX
#   TOTE_REDACT=1 install.sh PROJECT_DIR        no transcript text in tote's log, and no disk cache of checker responses
#   install.sh --remove PROJECT_DIR             take tote's hook out again
set -euo pipefail
command -v jq >/dev/null || { echo "install.sh needs jq" >&2; exit 1; }
remove=0
if [ "${1:-}" = --remove ]; then remove=1; shift; fi
here=$(cd "$(dirname "$0")" && pwd)
tote=$(cd "$here/../.." && pwd)
root=$(cd "${1:-$tote}" && pwd)
dest="$root/.claude/settings.local.json"
mkdir -p "$root/.claude"
[ -f "$dest" ] || echo '{}' > "$dest"
cp "$dest" "$dest.bak-$(date +%s)"
hook=$(sed "s#__TOTE__#$tote#g" "$here/settings.local.json" | jq '.hooks.Stop[0]')
if [ -n "${TOTE_SKIP_IF:-}" ]; then
  hook=$(jq --arg re "$TOTE_SKIP_IF" '.hooks[0].command = "TOTE_SKIP_IF=" + ($re | @sh) + " " + .hooks[0].command' <<<"$hook")
fi
if [ "${TOTE_REDACT:-}" = 1 ]; then
  hook=$(jq '.hooks[0].command = "TOTE_REDACT=1 " + .hooks[0].command' <<<"$hook")
fi
if [ "$remove" = 1 ]; then
  out=$(jq '.hooks.Stop = [(.hooks.Stop // [])[] | select(any(.hooks[]?; .command | test("tote\\.check")) | not)] | if .hooks.Stop == [] then del(.hooks.Stop) else . end' "$dest")
  printf '%s\n' "$out" > "$dest"
  echo "removed the tote Stop hook from $dest"
  exit 0
fi
out=$(jq --argjson h "$hook" '.hooks.Stop = ([(.hooks.Stop // [])[] | select(any(.hooks[]?; .command | test("tote\\.check")) | not)] + [$h])' "$dest")
printf '%s\n' "$out" > "$dest"
echo "wrote the tote Stop hook into $dest"
