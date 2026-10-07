#!/bin/sh
# check-writing: fail when an em dash or en dash appears in a source or
# documentation file (CLAUDE.md, writing rule 1).
#
# Default scope is a ratchet: files changed against $BASE (default
# origin/master), staged, unstaged or untracked. ALL=1 scans every tracked
# file; the repository is not clean under ALL=1 yet (ISS-024).
#
# The characters are built from their UTF-8 bytes so this script does not
# contain them, and so it works with BSD grep, which has no -P.
set -eu

BASE="${BASE:-origin/master}"
EM=$(printf '\342\200\224')
EN=$(printf '\342\200\223')

if [ "${ALL:-0}" = "1" ]; then
	list() { git ls-files; }
else
	list() {
		git diff --name-only --diff-filter=d "$BASE"...HEAD
		git diff --name-only --diff-filter=d HEAD
		git ls-files --others --exclude-standard
	}
fi

status=0
checked=0
for f in $(list | sort -u); do
	case "$f" in
	*.go | *.md | *.yaml | *.yml | *.tpl | *.sh | Makefile | Dockerfile) ;;
	*) continue ;;
	esac
	[ -f "$f" ] || continue
	checked=$((checked + 1))
	if grep -Hn -e "$EM" -e "$EN" "$f"; then
		status=1
	fi
done

echo "check-writing: scanned $checked files (base: $([ "${ALL:-0}" = 1 ] && echo all || echo "$BASE"))"
if [ "$status" -ne 0 ]; then
	echo "check-writing: FAIL, replace each dash above with a comma, colon or parentheses"
fi
exit "$status"
