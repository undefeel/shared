import QtQuick
import Quickshell
import Quickshell.Io

// Состояние демона sharedir. Держит `sharedir watch`, который печатает
// статус JSON-строкой при каждом изменении, и выполняет команды
// (pause/resume/pair) отдельным процессом.
Item {
  id: root

  property var settings: ({})

  readonly property string binary: {
    var configured = settings && settings.binary ? String(settings.binary) : ""
    return configured !== "" ? configured : Quickshell.env("HOME") + "/.local/bin/sharedir"
  }

  property var status: ({ running: false })
  readonly property bool running: status.running === true
  readonly property bool paused: running && status.paused === true
  readonly property bool syncing: running && status.syncing === true
  readonly property var peers: status.peers || []
  readonly property var candidates: status.candidates || []
  readonly property var kinds: status.kinds || []
  readonly property var days: status.days || []
  readonly property var recent: status.recent || []
  readonly property bool anyConnected: {
    for (var i = 0; i < peers.length; i++) if (peers[i].connected) return true
    return false
  }

  // Оптимистичная пауза, как у Dropbox: переключатель щёлкает сразу,
  // не дожидаясь следующей строки от watch. -1 — следуем реальности.
  property int _desiredPaused: -1
  readonly property bool active: running && (_desiredPaused === -1 ? !paused : _desiredPaused === 0)

  property string lastError: ""
  readonly property bool busy: controlProcess.running

  function apply(line) {
    var text = String(line || "").trim()
    if (text === "") return
    try {
      status = JSON.parse(text)
      if (_desiredPaused !== -1 && paused === (_desiredPaused === 1)) _desiredPaused = -1
    } catch (e) {
      lastError = "sharedir: " + e
    }
  }

  function run(args) {
    if (controlProcess.running) return
    lastError = ""
    controlProcess.command = [binary].concat(args)
    controlProcess.running = true
  }

  function setPaused(on) {
    _desiredPaused = on ? 1 : 0
    run([on ? "pause" : "resume"])
  }

  function toggle() { setPaused(active) }
  function pair(id) { run(["pair", id]) }

  function startDaemon() {
    Quickshell.execDetached(["systemctl", "--user", "start", "sharedir.service"])
  }

  function openFolder() {
    if (status.folder) Quickshell.execDetached(["xdg-open", status.folder])
  }

  function revealFile(name) {
    if (!status.folder || !name) return
    var parts = (status.folder + "/" + name).split("/")
    for (var i = 0; i < parts.length; i++) parts[i] = encodeURIComponent(parts[i])
    Quickshell.execDetached(["uwsm-app", "--", "nautilus", "--select", "file://" + parts.join("/")])
  }

  function copyId(pretty) {
    Quickshell.execDetached(["wl-copy", pretty])
  }

  Process {
    id: watchProcess
    command: [root.binary, "watch"]
    running: true
    stdout: SplitParser { onRead: function(data) { root.apply(data) } }
    onExited: function(exitCode) {
      // Бинарника нет или он упал — показываем «не запущен» и пробуем снова.
      root.status = ({ running: false })
      restartTimer.restart()
    }
  }

  Timer {
    id: restartTimer
    interval: 3000
    onTriggered: watchProcess.running = true
  }

  Process {
    id: controlProcess
    running: false
    command: []
    stderr: StdioCollector { id: controlStderr; waitForEnd: true }
    onExited: function(exitCode) {
      if (exitCode !== 0) {
        root._desiredPaused = -1
        root.lastError = String(controlStderr.text || "").trim()
      }
    }
  }
}
