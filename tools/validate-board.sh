#!/bin/sh
# Validates the row structure of KANZ_TASKS.md.
#
#   sh tools/validate-board.sh KANZ_TASKS.md
#
# WHY THIS IS IN THE REPO. It lived in .superpowers/sdd/, which carries a
# blanket `*` .gitignore because it is agent scratch. So the script was never
# committed: every session inherited whatever copy happened to be on disk, and
# `git clean -fdx` silently reverted it to an older, more broken version. A
# check the board's workflow depends on cannot live somewhere a routine cleanup
# deletes it.
#
# WHAT IT CATCHES. Board rows have 5 columns, so 6 pipes. A literal `|` in prose
# — writing 10^|exponent| rather than 10^abs(exponent) — splits a cell and
# corrupts the table. That has happened.
#
# TWO GENERATIONS OF THIS SCRIPT FAILED THE SAME WAY, WHICH IS WHY THE EXIT
# CODES BELOW MATTER MORE THAN THE OUTPUT.
#
#   1. The original printed MALFORMED and exited 0, so an `&& git add` chain
#      committed the broken row anyway. That is finding #8 on the board.
#   2. Its replacement — written to fix exactly that — read the board path from
#      $1 and, run with NO argument, had awk read empty stdin, match zero rows,
#      print "board OK:  rows" with an empty count, and exit 0. A caller who
#      omitted the argument got a green light over a board nobody had looked at.
#      One did, and reported "validator exit code: 0" as evidence the board was
#      well-formed. It was, by luck.
#
# So: a validator that cannot distinguish "everything is fine" from "I examined
# nothing" is the failure it exists to prevent. Every arm below exits non-zero
# for a reason, and callers must check the code rather than reading the text.
if [ -z "$1" ]; then
  echo "usage: validate-board.sh <path-to-KANZ_TASKS.md>" >&2
  echo "refusing to run with no argument: awk would read empty stdin, match zero rows, and PASS." >&2
  exit 2
fi
if [ ! -f "$1" ]; then
  echo "no such file: $1" >&2
  exit 2
fi

bad=$(awk '/^\| \*\*/ {n=gsub(/\|/,"|"); if (n!=6) print NR": "n" pipes: "substr($0,1,60)}' "$1")
if [ -n "$bad" ]; then
  echo "MALFORMED board rows:"; echo "$bad"; exit 1
fi

rows=$(awk '/^\| \*\*/ {c++} END {print c+0}' "$1")
# Zero rows means the row pattern stopped matching — the board was restructured,
# or the wrong file was passed. Either way this script is now asserting nothing,
# and saying so is more useful than printing OK.
if [ "$rows" -eq 0 ]; then
  echo "NO board rows matched in $1. Either the wrong file was passed, or the row" >&2
  echo "format changed and this validator now checks nothing. Fix the pattern rather" >&2
  echo "than deleting the check." >&2
  exit 3
fi

echo "board OK: $rows rows, 5 columns each"
