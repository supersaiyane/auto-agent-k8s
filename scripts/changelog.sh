#!/bin/sh
# Writes docs/wiki/19-changelog.md from git history; every count is computed here.
set -eu
bt='`' # a literal backtick, kept out of single-quoted strings (SC2016)
section() { # title range note
	title=$1 range=$2 note=$3
	n=$(git rev-list --count --no-merges "$range")
	first=$(git log --reverse --format=%ad --date=short --no-merges "$range" | head -1)
	last=$(git log -1 --format=%ad --date=short --no-merges "$range")
	types=$(git log --format=%s --no-merges "$range" | sed -n 's/^\([a-z]*\):.*/\1/p' | sort | uniq -c | sort -rn | awk '{printf "%s%s %s", sep, $1, $2; sep=", "}')
	printf '## %s\n\n%s\n\n%s commits, %s to %s.' "$title" "$note" "$n" "$first" "$last"
	[ -n "$types" ] && printf ' By type: %s.' "$types"
	printf '\n\n'
	if [ "${4:-list}" = list ]; then
		git log --reverse --format="- ${bt}%h${bt} %s" --no-merges "$range"
		printf '\n'
	fi
}
{
	printf '# Changelog\n\nGenerated from git history on %s by %smake changelog%s. Counts are\nmeasured with %sgit rev-list --count --no-merges%s; nothing here is written by\nhand. The project has no release tags yet.\n\n' "$(date +%Y-%m-%d)" "$bt" "$bt" "$bt" "$bt"
	section "Unreleased (branch plan-002 after PR #2)" "581c215..HEAD" "PLAN-002 phase 11 continued: watch scope and fix scope (ADR-002), the Settings tab, shutdown order, check-config, docs."
	section "PR #2: PLAN-002 phases 8 to 11 (merged 2026-10-08)" "b72e21b..581c215" "Config in one place, coverage gates, native detectors with the fix ladder, node and controller roles (ADR-001), dashboard rebuilt, safe deployment scripts."
	section "PR #1: PLAN-001 safety hardening (merged 2026-10-07)" "5828dbc..b72e21b" "One mutation gate, dry-run by default, RBAC matching the code, redaction, authenticated endpoints."
	section "Original history" "5828dbc" "The agent as first written, before the plans. Claims made in that period (detector counts, a \"production release\") were not measured and are not repeated here." count
} > docs/wiki/19-changelog.md
