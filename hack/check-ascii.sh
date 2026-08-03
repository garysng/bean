#!/usr/bin/env bash
# Fails if CJK text appears anywhere it should not.
#
# The rule: prose documentation may be written in any language, but code, commit
# messages and branch names may not. A contributor who cannot read Chinese should
# be able to work on every file that is not documentation, and `git log` should
# stay readable to everyone — which it stops being the moment half the history
# needs translating.
#
# Documentation is exempt by path rather than by content, so a Chinese translation
# lives under docs/zh/ and nothing else has to know about it.
#
# Only CJK ranges are rejected, not "anything non-ASCII": em-dashes, arrows and
# box-drawing characters are used deliberately in comments and diagrams
# throughout, and banning those would be a different and much more annoying rule.
#
# Usage:
#   hack/check-ascii.sh            check tracked files
#   hack/check-ascii.sh --commits  also check commit messages not yet pushed
set -uo pipefail

CHECK_COMMITS=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --commits) CHECK_COMMITS=1; shift ;;
    -h|--help) sed -n '2,18p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 64 ;;
  esac
done

status=0
say() { printf '%s\n' "$*"; }

# The matcher lives in Python rather than grep because grep -P is unavailable on
# BSD grep, and because the pattern is written with codepoint escapes — this file
# has to pass its own check, so it cannot contain a literal CJK character.
PYMATCH='import re,sys
pat = re.compile("[\u3040-\u30ff\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff\uac00-\ud7af]")'

# cjk_in_file prints up to three offending lines and exits 0 when any were found.
cjk_in_file() {
  python3 -c "$PYMATCH
path = sys.argv[1]
try:
    raw = open(path, 'rb').read()
except OSError:
    sys.exit(1)
if b'\0' in raw[:8192]:
    sys.exit(1)                      # binary
try:
    text = raw.decode('utf-8')
except UnicodeDecodeError:
    sys.exit(1)
hits = [(i, l) for i, l in enumerate(text.splitlines(), 1) if pat.search(l)]
for i, l in hits[:3]:
    print('%d:%s' % (i, l.strip()[:100]))
sys.exit(0 if hits else 1)" "$1"
}

# cjk_in_string exits 0 when the argument contains CJK.
cjk_in_string() {
  python3 -c "$PYMATCH
sys.exit(0 if pat.search(sys.argv[1]) else 1)" "$1"
}

# ---- tracked files ---------------------------------------------------------
# git ls-files rather than find: generated code and ignored files are not ours to
# police, and this way the exemption list stays short.
while IFS= read -r f; do
  case "$f" in
    docs/*|*.md|internal/gen/*) continue ;;
  esac
  [[ -f "$f" ]] || continue
  if hits=$(cjk_in_file "$f"); then
    if [[ "$status" == "0" ]]; then
      say "CJK text outside documentation:"
    fi
    status=1
    say "  $f"
    sed 's/^/      /' <<< "$hits"
  fi
done < <(git ls-files)

if [[ "$status" == "1" ]]; then
  say ""
  say "Code, scripts and configuration must be ASCII. Prose belongs in docs/,"
  say "with translations under docs/zh/."
fi

# ---- commit messages -------------------------------------------------------
if [[ "$CHECK_COMMITS" == "1" ]]; then
  # Unpushed commits only. Rewriting published history to fix a message costs
  # more than the message is worth, so the line is drawn where a rewrite is free.
  if git rev-parse --verify --quiet origin/main >/dev/null; then
    bad=""
    while IFS= read -r line; do
      [[ -n "$line" ]] || continue
      if cjk_in_string "$line"; then
        bad+="$line"$'\n'
      fi
    done < <(git log --format='%h %s' origin/main..HEAD 2>/dev/null)
    if [[ -n "$bad" ]]; then
      status=1
      say ""
      say "Unpushed commits with CJK messages:"
      printf '%s' "$bad" | sed 's/^/  /'
      say ""
      say "Rewrite them with: git rebase -i origin/main"
    fi
  fi
fi

# ---- branch name -----------------------------------------------------------
branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || true)
if [[ -n "$branch" ]] && cjk_in_string "$branch"; then
  status=1
  say ""
  say "Branch name is not ASCII: $branch"
fi

if [[ "$status" == "0" ]]; then
  say "ASCII check passed"
fi
exit "$status"
