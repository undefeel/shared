import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'

const source = readFileSync(new URL('../Logic.js', import.meta.url), 'utf8')
  .replace(/^\.pragma library\s*/, '')
const logic = new Function(`${source}\nreturn { parseStatus, folderError, folderErrorText, scanErrorText, confirmArgv, cliResult }`)()

const oldFixture = '{"running":true,"paused":false,"peers":[]}'
const blockedFixture = JSON.stringify({
  running: true,
  paused: false,
  syncing: true,
  folder_error: { code: 'root_empty', message: 'empty folder', token: 'tkn-42' }
})

test('legacy status fixture has no folder error', () => {
  const status = logic.parseStatus(oldFixture)
  assert.equal(status.running, true)
  assert.equal(logic.folderError(status), null)
})

test('blocked status fixture stays visible while running and explains root_empty', () => {
  const status = logic.parseStatus(blockedFixture)
  assert.equal(status.running, true)
  assert.deepEqual(logic.folderError(status), status.folder_error)
  assert.equal(logic.folderErrorText(status, 'en'), 'The folder is empty')
  assert.equal(logic.folderErrorText(status, 'ru'), 'Папка пуста')
})

test('reset confirmation requires explicit action and builds argv entries', () => {
  const status = logic.parseStatus(blockedFixture)
  assert.deepEqual(logic.confirmArgv('/path with spaces/sharedir', status, 'reset'), [
    '/path with spaces/sharedir', 'folder-confirm', '--action', 'reset', '--token', 'tkn-42'
  ])
  assert.equal(logic.confirmArgv('sharedir', status, 'other'), null)
})

test('rebind confirmation passes the exact token as one argv entry', () => {
  const status = logic.parseStatus(blockedFixture)
  status.folder_error.token = 'token; $HOME'
  assert.deepEqual(logic.confirmArgv('sharedir', status, 'rebind'), [
    'sharedir', 'folder-confirm', '--action', 'rebind', '--token', 'token; $HOME'
  ])
})

test('stale token / HTTP 409 keeps the folder blocked and asks for a fresh status', () => {
  const status = logic.parseStatus(blockedFixture)
  const result = logic.cliResult(status, 'rebind', 1, 'stale folder confirmation token')
  assert.equal(result.status, status)
  assert.equal(logic.folderError(result.status).token, 'tkn-42')
  assert.equal(result.lastError, 'stale folder confirmation token')
  assert.equal(result.lastResult, 'folder-confirm-stale')
})

test('HTTP 409 response is recognized as a stale confirmation', () => {
  const status = logic.parseStatus(blockedFixture)
  assert.equal(logic.cliResult(status, 'reset', 1, 'Conflict (409)').lastResult, 'folder-confirm-stale')
})

test('failed confirmation remains an error even without CLI stderr', () => {
  const status = logic.parseStatus(blockedFixture)
  const result = logic.cliResult(status, 'reset', 1, '')
  assert.ok(logic.folderError(result.status))
  assert.equal(result.lastError, 'Command failed')
  assert.equal(result.lastResult, 'folder-confirm-failed')
})

test('successful reset reports its result while the old status awaits refresh', () => {
  const status = logic.parseStatus(blockedFixture)
  const result = logic.cliResult(status, 'reset', 0, '')
  assert.equal(result.lastResult, 'folder-reset-success')
  assert.equal(result.lastError, '')
  assert.ok(logic.folderError(result.status))
})

test('successful rebind reports its result while the old status awaits refresh', () => {
  const status = logic.parseStatus(blockedFixture)
  const result = logic.cliResult(status, 'rebind', 0, '')
  assert.equal(result.lastResult, 'folder-rebind-success')
  assert.equal(result.lastError, '')
  assert.ok(logic.folderError(result.status))
})

test('scan_error has a readable localized message without becoming a folder block', () => {
  const status = logic.parseStatus(JSON.stringify({
    running: true,
    scan_error: { message: 'permission denied', path: 'docs/a.txt' }
  }))
  assert.equal(logic.folderError(status), null)
  assert.equal(logic.scanErrorText(status, 'en'), 'Scan error: permission denied · docs/a.txt')
  assert.equal(logic.scanErrorText(status, 'ru'), 'Ошибка сканирования: permission denied · docs/a.txt')
})

test('resume CLI success does not clear the root block from current status', () => {
  const status = logic.parseStatus(blockedFixture)
  const result = logic.cliResult(status, 'resume', 0, '')
  assert.equal(result.status, status)
  assert.ok(logic.folderError(result.status))
})
