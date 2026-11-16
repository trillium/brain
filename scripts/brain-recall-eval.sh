#!/usr/bin/env bash
# brain-recall-eval.sh — score `brain recall` against the fixed eval set in
# docs/brain/recall-eval.json, next to the baseline `brain search`.
#
# A case passes when one of its target ids is in the top N (fixture "top")
# of the verb's ranked output. Both verbs are run read-only against the live
# store named by the fixture, through that store's wrapper. The wrapper honors
# BD_BIN, so point it at a scratch build to score an uncommitted binary:
#
#   BD_BIN=/path/to/bd scripts/brain-recall-eval.sh
#   scripts/brain-recall-eval.sh --json      # machine-readable per-case rows
#
# Exit 0 when recall scores at or above the pass bar (15 of 20), 1 otherwise.
set -euo pipefail

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
fixture="${RECALL_EVAL_FIXTURE:-$here/docs/brain/recall-eval.json}"
pass_bar="${RECALL_EVAL_PASS_BAR:-15}"
json_out=0
[ "${1:-}" = "--json" ] && json_out=1

command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }
store="$(jq -r '.store' "$fixture")"
top="$(jq -r '.top' "$fixture")"
wrapper="${RECALL_EVAL_WRAPPER:-$store}"
command -v "$wrapper" >/dev/null || { echo "store wrapper '$wrapper' not on PATH" >&2; exit 2; }

# top_ids <verb> <query> — the first N ids a verb returns, one per line.
top_ids() {
  local verb="$1" query="$2" output status validate ids
  case "$verb" in
    recall)
      validate='type == "object" and (.hits | type == "array")'
      if output="$("$wrapper" recall --json --top "$top" "$query")"; then status=0; else status=$?; fi
      ;;
    search)
      validate='type == "array"'
      if output="$("$wrapper" search --json --limit "$top" "$query")"; then status=0; else status=$?; fi
      ;;
    *) echo "unknown verb: $verb" >&2; return 2 ;;
  esac
  if ! printf '%s' "$output" | jq -e "$validate" >/dev/null; then
    echo "$verb returned invalid JSON" >&2
    if [ "$status" -ne 0 ]; then
      return "$status"
    fi
    return 1
  fi
  ids="$(case "$verb" in
    recall) printf '%s' "$output" | jq -r '.hits[]?.id' ;;
    search) printf '%s' "$output" | jq -r '.[]?.id' ;;
  esac)"
  if [ "$status" -ne 0 ] && [ -n "$ids" ]; then
    echo "$verb returned ids with exit status $status" >&2
    return "$status"
  fi
  printf '%s\n' "$ids"
}

rows=()
recall_ok=0; search_ok=0; n=0
while IFS=$'\t' read -r query targets where; do
  n=$((n+1))
  r_ids="$(top_ids recall "$query" | head -n "$top" | paste -sd, -)"
  s_ids="$(top_ids search "$query" | head -n "$top" | paste -sd, -)"
  r_hit=0; s_hit=0
  for t in ${targets//,/ }; do
    case ",$r_ids," in *",$t,"*) r_hit=1 ;; esac
    case ",$s_ids," in *",$t,"*) s_hit=1 ;; esac
  done
  recall_ok=$((recall_ok + r_hit)); search_ok=$((search_ok + s_hit))
  rows+=("$(jq -cn --arg q "$query" --arg t "$targets" --arg w "$where" \
      --arg r "$r_ids" --arg s "$s_ids" --argjson rh "$r_hit" --argjson sh "$s_hit" \
      '{query:$q, targets:($t|split(",")), where:$w, recall:{hit:($rh==1), top:($r|split(",")|map(select(.!="")))}, search:{hit:($sh==1), top:($s|split(",")|map(select(.!="")))}}')")
done < <(jq -r '.cases[] | [.query, (.targets|join(",")), .where] | @tsv' "$fixture")

if [ "$json_out" = 1 ]; then
  printf '%s\n' "${rows[@]}" | jq -s --argjson n "$n" --argjson r "$recall_ok" --argjson s "$search_ok" --argjson top "$top" \
    '{cases:., total:$n, top:$top, recall_in_top:$r, search_in_top:$s}'
else
  printf '%-44s  %-8s  %-7s  %-7s\n' "query" "where" "recall" "search"
  printf '%s\n' "${rows[@]}" | jq -r '[.query, .where, (if .recall.hit then "hit" else "miss" end), (if .search.hit then "hit" else "miss" end)] | @tsv' \
    | while IFS=$'\t' read -r q w r s; do printf '%-44s  %-8s  %-7s  %-7s\n' "$q" "$w" "$r" "$s"; done
  echo
  echo "brain recall: $recall_ok/$n in top $top   (pass bar $pass_bar)"
  echo "brain search: $search_ok/$n in top $top   (baseline)"
fi
[ "$recall_ok" -ge "$pass_bar" ]
