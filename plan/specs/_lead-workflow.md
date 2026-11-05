<!-- Canonical 'Lead workflow' section for L3 specs; replace <ID>. -->
## Lead workflow (mandatory — you are the LEAD, not the implementer)
Smart model plans, light model implements, smart model reviews. The coordinator does not take part.
1. Plan (cheap, most important): read this spec and the design, then write `plan/specs/<ID>.plan.md` — files and functions to add/change, signatures, data flow, invariants and lock order, exact test list with scenarios, and pitfalls. Precise enough that a weaker model cannot go in the wrong direction. Commit it.
2. Delegate: run `plan/tools/lead.sh <ID> implement`, or, if the plan splits into parts with disjoint files, up to 3 parallel parts (`plan/tools/lead.sh <ID> part <name> <part.md>` each, then `plan/tools/lead.sh --wait-all <dispatches>`; parts do not commit — you run the full acceptance and commit). A large plan may also be delivered as sequential phases (one `implement` per committed phase plan, at most 3). The implementer (luna by default) implements the plan in this worktree, runs acceptance, commits and reports. Exit code 3: answer its question (`orca orchestration reply --id <id> --body ...`) if it asked one, then re-run `plan/tools/lead.sh --wait <dispatch>`.
3. Review: run the acceptance commands yourself, then the cheap first pass `plan/tools/self-review.sh <ID>` (Gemini), then review the whole diff yourself (correctness, races, durability, plan conformance, tests really testing the claims). Reject Gemini findings that contradict the spec or the owner decisions.
4. Fix rounds: if changes are needed, write the notes to a file under `$(git rev-parse --git-dir)/` and run `plan/tools/lead.sh <ID> fix <N> <file>` (N = 1, then 2). At most 2 fix rounds.
5. Never end your turn while a child worker (implementer or reviewer) is running: Orca does not wake you when it reports. On exit code 3 immediately re-run the `--wait` command.
6. Takeover: if after 2 fix rounds the review still does not pass, finish the task yourself (that is the safety net against endless iterations).
7. worker_done: plan file, number of implementer/fix rounds, whether you took over, every finding (Gemini and yours) with its disposition, acceptance results.

Extra rules (owner decision 2026-11-05: luna first, then terra):
- The default implementer is luna (gpt-6-luna high, the `lead.sh` default). Cut the plan into pieces of ~300 lines in 1-3 files each (sequential phases or disjoint parts), each naming exact files, functions, signatures and tests.
- Escalate a piece to terra (`LEAD_IMPL=terra plan/tools/lead.sh <ID> implement`, up to ~600 lines) when luna stopped 3 times without finishing (`lead.sh` tells you), sent worker_done failed, or delivered a piece that fails acceptance or review. Commit the correct part of luna's work first; terra continues from there.
- If the implementer ends its turn without reporting, `lead.sh` continues it automatically; just keep waiting.
- Takeover is mandatory and complete: after the fix rounds you finish everything that is missing yourself. worker_done with outcome failed is acceptable only for a concrete technical blocker you name.
