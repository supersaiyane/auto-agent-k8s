#!/bin/sh
# check-writing: fail when an em dash or en dash appears in a source or
# documentation file (CLAUDE.md, writing rule 1).
#
# Scans every tracked and untracked file. The repository was cleaned on
# 2026-10-07 (ISS-024), so there is no ratchet any more.
#
# The characters are built from their UTF-8 bytes so this script does not
# contain them, and so it works with BSD grep, which has no -P.
set -eu

EM=$(printf '\342\200\224')
EN=$(printf '\342\200\223')

list() {
	git ls-files
	git ls-files --others --exclude-standard
}

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

echo "check-writing: scanned $checked files"
if [ "$status" -ne 0 ]; then
	echo "check-writing: FAIL, replace each dash above with a comma, colon or parentheses"
fi
exit "$status"
