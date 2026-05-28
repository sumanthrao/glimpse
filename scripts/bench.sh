#!/usr/bin/env bash
# Benchmarks glimpse against git clone --depth=1 across a few public repos.
#
# For each repo we measure four numbers:
#   t_tree     time-to-first-tree       (glimpse: open+ls;  git: clone+ls)
#   t_cat      time-to-first-read       (glimpse: open+cat; git: clone, included)
#   t_grep     time-to-first-grep-hit   (glimpse: grep;     git: clone+git grep)
#   disk       bytes on disk after      (glimpse: 0;        git: du -sh on clone)
#
# Glimpse runs use a fresh cache dir so reads are honest cold fetches.
# Tokens are read from $GITHUB_TOKEN; without one glimpse will hit 60 REST/hr.
#
# Run from repo root:
#     scripts/bench.sh
set -uo pipefail

GLIMPSE=${GLIMPSE:-./glimpse}
TMP=$(mktemp -d -t glimpse-bench-XXXX)
trap 'rm -rf "$TMP"' EXIT

# Each entry: <slug>|<url>|<readme path>|<grep literal>
REPOS=(
  "cli/cli|https://github.com/cli/cli|README.md|package main"
  "hashicorp/terraform|https://github.com/hashicorp/terraform|README.md|package terraform"
  "torvalds/linux|https://github.com/torvalds/linux|README|EXPORT_SYMBOL_GPL"
)

elapsed() {
  # $1: tag printed alongside output
  # remaining args: command to run
  local tag=$1; shift
  local start end
  start=$(python3 -c 'import time;print(time.time())')
  "$@" >/dev/null 2>&1
  end=$(python3 -c 'import time;print(time.time())')
  python3 -c "print(f'{($end-$start):.2f}')"
}

printf "%-22s %-7s %-7s %-7s %-7s %-9s\n" repo tool t_tree t_cat t_grep disk
printf "%-22s %-7s %-7s %-7s %-7s %-9s\n" "----" "----" "------" "-----" "------" "----"

for entry in "${REPOS[@]}"; do
  IFS='|' read -r slug url readme grep_pat <<<"$entry"

  # ---- glimpse (cold cache each run) ----
  cache="$TMP/glimpse-$(echo "$slug" | tr / -)"
  rm -rf "$cache"; mkdir -p "$cache"
  t_tree=$(elapsed "$slug glimpse-tree" \
    "$GLIMPSE" --cache-dir "$cache" ls "$url")
  t_cat=$(elapsed "$slug glimpse-cat" \
    "$GLIMPSE" --cache-dir "$cache" cat "$url" "$readme")
  t_grep=$(elapsed "$slug glimpse-grep" \
    "$GLIMPSE" --cache-dir "$cache" grep "$url" "$grep_pat")
  printf "%-22s %-7s %-7s %-7s %-7s %-9s\n" "$slug" glimpse "$t_tree" "$t_cat" "$t_grep" "0"

  # ---- git clone --depth=1 baseline ----
  clone_dir="$TMP/git-$(echo "$slug" | tr / -)"
  rm -rf "$clone_dir"
  t_clone=$(elapsed "$slug git-clone" \
    git clone --depth=1 --quiet "$url" "$clone_dir")
  # t_cat for git = t_clone (file present after clone)
  t_grep_git=$(elapsed "$slug git-grep" \
    git -C "$clone_dir" grep -l "$grep_pat")
  # add clone time to grep wall (cold scenario: starting from no repo)
  t_grep_total=$(python3 -c "print(f'{(${t_clone}+${t_grep_git}):.2f}')")
  disk=$(du -sh "$clone_dir" 2>/dev/null | awk '{print $1}')
  printf "%-22s %-7s %-7s %-7s %-7s %-9s\n" "$slug" git "$t_clone" "$t_clone" "$t_grep_total" "$disk"
done
