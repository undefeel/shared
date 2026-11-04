.pragma library

// Чистая логика status/control, без зависимости от Qt и Quickshell.
function parseStatus(value) {
  try {
    var status = typeof value === "string" ? JSON.parse(value) : value
    return status && typeof status === "object" && !Array.isArray(status) ? status : null
  } catch (e) {
    return null
  }
}

function folderError(status) {
  var error = status && status.folder_error
  return status && status.running === true && error && typeof error.token === "string" && error.token !== "" ? error : null
}

function folderErrorText(status, language) {
  var error = folderError(status)
  if (!error) return ""
  var ru = language === "ru"
  var messages = ru ? {
    root_empty: "Папка пуста",
    root_unavailable: "Папка недоступна",
    root_changed: "Папка изменилась",
    commit_failed: "Не удалось записать изменения на диск"
  } : {
    root_empty: "The folder is empty",
    root_unavailable: "The folder is unavailable",
    root_changed: "The folder has changed",
    commit_failed: "Could not safely save changes"
  }
  return messages[error.code] || (ru ? "Папка остановлена" : "Folder stopped")
}

function scanErrorText(status, language) {
  var error = status && status.running === true && status.scan_error
  if (!error) return ""
  var prefix = language === "ru" ? "Ошибка сканирования" : "Scan error"
  var detail = String(error.message || "")
  if (error.path) detail += (detail ? " · " : "") + String(error.path)
  return detail ? prefix + ": " + detail : prefix
}

function confirmArgv(binary, status, action) {
  var error = folderError(status)
  if (!error || (action !== "reset" && action !== "rebind")) return null
  return [String(binary), "folder-confirm", "--action", action, "--token", error.token]
}

function cliResult(status, operation, exitCode, stderr) {
  var failed = exitCode !== 0
  var message = String(stderr || "Command failed")
  var result = ""
  if (operation === "reset" || operation === "rebind") {
    if (!failed) result = "folder-" + operation + "-success"
    else result = /stale folder confirmation token|\b409\b/i.test(message)
      ? "folder-confirm-stale" : "folder-confirm-failed"
  }
  return {
    status: status,
    lastError: failed ? message : "",
    lastResult: result
  }
}
