# Test policy (sharedir)

Every worker that changes code follows this file. The spec may tighten it, never loosen it silently.

## 1. Tests ship with the change
- Every behaviour change comes with new or updated tests in the same commit. Pure refactors and docs-only changes are exempt; say so in `worker_done`.
- A bug fix starts with a regression test that fails before the fix. Report in `worker_done` that you saw it fail.
- New logic goes where it can be tested: a pure function or a small type, not inline in a handler, a view or `main`.

## 2. What to test, by kind of change
| Change | Required tests |
|---|---|
| Disk writes, deletes, renames, recovery (`internal/folder`) | Interruption between steps (crash/restart scenario) loses no data; a transient I/O or network error never becomes a delete or tombstone; no escape from the root, no symlink follow. |
| Concurrency (watcher, Scan, runPeer, downloads, locks) | A test that runs the competing paths named in the spec together under `-race`. |
| Wire protocol, network (`transport`, `relay`, `discovery`) | Old/new version compatibility or a clean refusal; truncated and malformed messages are rejected without panic. New parsers get fuzz seeds. |
| CLI, config, i18n | Table tests: valid input, invalid input, error text/exit code. |
| macOS app (Swift) | Logic only: `Store`, `Snapshot`, `ControlClient` and other non-view code, in `macos/tests/`, run by `make test-macos`. Views and layout are not tested. |
| Omarchy widget (QML/JS) | Logic only: `omarchy/sharedir/Logic.js` (and other `.js` modules), in `omarchy/sharedir/tests/`, run by `make test-omarchy`. QML layout is not tested; logic in QML is moved into a `.js` module. |
| Shell scripts | Executed with stubs, never grep for strings. |
| Performance-sensitive paths | The perf harness `SHAREDIR_PERF=1 go test -count=1 -run Perf ./internal/app/ -v` shows no regression. |

## 3. How tests are written
- Test observable behaviour (files on disk, messages on the wire, returned values), not private fields or call order.
- No `time.Sleep` to wait for something: poll a condition with a deadline. Sleeps only to create a real time gap (mtime, timers), and as short as possible.
- `t.TempDir()` for files, loopback only for network, no external services, no fixed ports.
- Deterministic: fixed random seeds, no dependence on test order or wall clock.
- Strictness matches the requirement: a correct alternative implementation must pass. If the spec allows several outcomes, the test accepts all of them.
- A new test that takes more than ~2 s needs a reason in `worker_done`; heavy scenarios honour `testing.Short()`.

## 4. Forbidden without explicit permission in the spec
- Deleting or weakening an existing test, or changing its expected values to match new behaviour.
- `t.Skip` without a concrete reason (platform, missing tool) in the message.
- Raising timeouts to hide a flaky test.
List every touched existing test and why in `worker_done`.

## 5. Acceptance at merge (`plan/tools/merge.sh`)
- Always: `go vet` (native and `GOOS=linux`), `go test -race ./...`, `make build`.
- Changes under `macos/`: `make test-macos`. Changes under `omarchy/`: `make test-omarchy`.
- Warning (not failure) if production Go code changed without any `_test.go` change; the coordinator then checks the `worker_done` justification.
