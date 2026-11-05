# Test policy (sharedir)

Goal: few, high-value tests and few test runs. Worker time and model limits are the scarce resource.
The spec may tighten this, never loosen it silently.

## 1. Write only the tests the spec names
- The spec lists 1–3 test scenarios in Observable acceptance. Write exactly those. Do not add extra scenarios, edge-case matrices or "while I'm here" tests.
- If no scenario is listed and the change is not in the risk list below, write no new test.
- Prefer extending an existing test (one more table row, one more assertion) over a new test function or fixture.
- A bug fix in a risk area gets one regression test that fails before the fix.

## 2. Risk list (tests are required here)
| Area | The one scenario that matters |
|---|---|
| Disk writes, deletes, renames, recovery (`internal/folder`) | Data survives an interruption between steps; a transient error never becomes a delete or tombstone. |
| Concurrency (watcher, Scan, runPeer, downloads, locks) | The competing paths named in the spec run together under `-race`. |
| Wire protocol (`transport`, `relay`) | A malformed or incompatible message is rejected without panic. |

Everything else (UI logic in Swift/JS, CLI, config, i18n, discovery, docs) — tests are optional; add one only if the spec asks.
Shell scripts, when tested, are executed with stubs, never grep for strings.

## 3. How often to run tests
- While working: run only the tests you touch, in your package: `go test -count=1 -run 'TestName' ./internal/<pkg>/`. No `-race`, no `./...`.
- Once, before the commit: the full acceptance from the spec (`go vet`, `go test -race ./...`, build). If it fails, fix and rerun only the failing package; the full run happens once more at merge anyway.
- After a review fix round: rerun only the affected packages, not the full suite.
- The perf harness (`SHAREDIR_PERF=1 …`) runs only when the spec is a performance task.
- Cut test output: `| tail -40`.

## 4. How tests are written
- Test observable behaviour (files on disk, messages, returned values), not private fields.
- No `time.Sleep` to wait for something: poll a condition with a short deadline. Keep each new test under ~1 s.
- `t.TempDir()`, loopback only, no fixed ports, deterministic.
- A correct alternative implementation must pass.

## 5. Forbidden without permission in the spec
- Deleting or weakening an existing test, or changing its expected values to fit new behaviour.
- `t.Skip` without a concrete reason; raising timeouts to hide flakiness.
List any touched existing test and why in `worker_done`.

## 6. Merge (`plan/tools/merge.sh`)
The coordinator runs the full acceptance once: `go vet` (native and Linux), `go test -race ./...`, `make build`; plus `make test-macos` / `make test-omarchy` when `macos/` / `omarchy/` changed.
