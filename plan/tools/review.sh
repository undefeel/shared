#!/usr/bin/env bash
# Ревью задачи (только чтение) в worktree её воркера: plan/tools/review.sh <ID> [luna|luna-low|gemini-medium(=flash-high)]
set -euo pipefail
ROOT="$(git rev-parse --show-toplevel)"; cd "$ROOT"
ID="$1"; EF=medium; [[ "${2:-luna}" == luna-low ]] && EF=low
AGENT=(--agent codex --model gpt-6-luna --effort "$EF")
[[ "${2:-}" == gemini-medium ]] && AGENT=(--agent antigravity --model gemini-3.8-flash-high)
WT="$(dirname "$ROOT")/t$ID"; BR="undefeel/t$ID"
mkdir -p plan/specs/review; S="plan/specs/review/$ID.md"
cat > "$S" <<SPEC
# Review $ID (read-only)

## Target
Branch \`$BR\` in the current worktree, commits: $(git log --format='%h' main.."$BR" | tr '\n' ' ').
Requirements: \`plan/specs/$ID.md\`; overall design: \`plan/design/folder.md\` (the "owner decision" section overrides the rest) and, for stage 04, \`plan/design/perf.md\` (its "owner decisions" section overrides the rest). The design docs are in Russian.

## Change
Review \`git diff main...$BR\` against the spec and the design, and look for bugs: escaping the root or following symlinks, fd leaks, lock order scanMu → saveMu → mu, races with watcher/Scan/multiple runPeer/downloads, a transient error turning into a delete or tombstone, data loss on a crash between steps, wire-format changes of \`/index\` and \`/file\`. Check that the tests required by the spec exist and actually test what they claim (not trivially passing).

## Constraints
Do not change code, do not commit, do not re-run tests. Answer in English in the \`worker_done\` body: a list of \`[blocker|major|minor] file:line — issue — how to fix\`, or "no findings". blocker/major only with a concrete execution path.

## Ownership
None (read-only).

## Observable acceptance
Report in the \`worker_done\` body.
SPEC
orca orchestration worker-start --spec-file "$S" --task-title "Review $ID" --worktree "path:$WT" \
  "${AGENT[@]}" --json | jq -r '.result.dispatchId // .'
