import QtQuick
import QtQuick.Controls
import QtQuick.Layouts
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Commons as Commons
import qs.Ui
import "Strings.js" as S
import "Logic.js" as Logic

// sharedir в баре Omarchy: виджет-статус и панель в духе Tailscale/Dropbox —
// hero с переключателем, вкладки «Устройства» и «Активность».
Panel {
  id: root
  moduleName: "sharedir"
  ipcTarget: "sharedir"
  manageIpc: false

  readonly property string lang: S.lang(Quickshell.env("LC_ALL") || Quickshell.env("LC_MESSAGES") || Quickshell.env("LANG"))
  readonly property color foreground: bar ? bar.foreground : Commons.Color.foreground
  readonly property color urgent: bar ? bar.urgent : Commons.Color.urgent
  readonly property color dim: Qt.darker(foreground, 1.4)
  readonly property string fontFamily: bar ? bar.fontFamily : Style.font.family

  property string tab: "devices"
  property string pairingId: ""
  property string folderConfirmAction: ""

  function tr(key, args) { return S.t(root.lang, key, args) }

  // Фаза для иконки и hero — та же логика, что у приложения macOS.
  readonly property string phase: {
    if (!sync.running) return "down"
    if (sync.folderError) return "blocked"
    if (!sync.active) return "paused"
    if (sync.peers.length === 0) return "nopeers"
    if (sync.syncing) return "syncing"
    return sync.anyConnected ? "synced" : "offline"
  }

  readonly property string barGlyph: {
    switch (phase) {
    case "synced": return "󰉋"   // md-folder
    case "syncing": return "󰴋"  // md-folder_sync
    case "down": return "󰷌"     // md-folder_alert
    case "blocked": return "󰷌"  // md-folder_alert
    default: return "󰉖"         // md-folder_outline
    }
  }

  readonly property string heroMeta: {
    switch (phase) {
    case "synced": return tr("Up to date")
    case "syncing": return tr("Syncing")
    case "paused": return tr("Paused")
    case "nopeers": return tr("No devices")
    case "down": return tr("Not running")
    case "blocked": return tr("Folder stopped")
    default: return tr("Offline")
    }
  }

  readonly property string statusLine: {
    if (sync.lastError !== "") return sync.lastError
    if (sync.folderError) return Logic.folderErrorText(sync.status, root.lang)
    var scanError = Logic.scanErrorText(sync.status, root.lang)
    if (scanError !== "") return scanError
    switch (phase) {
    case "down": return tr("The sharedir daemon isn't running.")
    case "paused": return tr("Changes pile up locally and sync after you resume.")
    case "nopeers": return tr("Pair a device below, then do the same on it.")
    case "offline": return tr("No peers on the network. Make sure they are on.")
    }
    for (var i = 0; i < sync.peers.length; i++) {
      var p = sync.peers[i].progress
      if (p) return tr("Receiving %1 of %2 · %3 left", [p.done_files, p.files, S.bytes(p.bytes - p.done_bytes)])
    }
    var online = 0
    for (var j = 0; j < sync.peers.length; j++) if (sync.peers[j].connected) online++
    return tr("%1 online · %2 in folder", [online, S.bytes(sync.status.bytes)])
  }

  implicitWidth: button.implicitWidth
  implicitHeight: button.implicitHeight

  onOpenedChanged: if (opened) {
    pairingId = ""
    if (panelFlick) panelFlick.contentY = 0
    Qt.callLater(function() { keyCatcher.forceActiveFocus() })
  }

  Service {
    id: sync
    settings: root.settings
  }

  IpcHandler {
    target: root.ipcTarget
    function open(): void { root.open() }
    function close(): void { root.close() }
    function toggle(): void { root.toggle() }
    function pause(): string { sync.setPaused(true); return "ok" }
    function resume(): string { sync.setPaused(false); return "ok" }
    function status(): string { return root.phase }
  }

  BarIconButton {
    id: button
    anchors.fill: parent
    bar: root.bar
    text: root.barGlyph
    opacity: root.phase === "synced" || root.phase === "syncing" ? 1.0 : 0.6
    onPressed: function(buttonCode) {
      if (buttonCode === Qt.RightButton) sync.toggle()
      else if (buttonCode === Qt.MiddleButton) sync.openFolder()
      else root.toggle()
    }
  }

  KeyboardPanel {
    id: panel
    anchorItem: button
    owner: root
    bar: root.bar
    open: root.opened
    focusTarget: keyCatcher
    contentWidth: panel.fittedContentWidth(Style.space(380))
    contentHeight: panel.fittedContentHeight(column.implicitHeight, Style.space(560))

    PanelKeyCatcher {
      id: keyCatcher
      anchors.fill: parent
      onMoveRequested: function(dx, dy) {
        if (dx > 0) root.tab = "activity"
        else if (dx < 0) root.tab = "devices"
      }
      onCloseRequested: root.close()
      onTabRequested: function(direction) { root.switchPanel(direction) }
      onTextKey: function(t) {
        if (t === "p" || t === "P") sync.toggle()
        else if (t === "o" || t === "O") sync.openFolder()
      }

      Flickable {
        id: panelFlick
        anchors.fill: parent
        contentWidth: width
        contentHeight: column.implicitHeight
        clip: true
        boundsBehavior: Flickable.StopAtBounds
        flickableDirection: Flickable.VerticalFlick
        interactive: contentHeight > height
        ScrollBar.vertical: ScrollBar { policy: ScrollBar.AsNeeded }

        Column {
          id: column
          width: panelFlick.width
          spacing: Style.space(12)

          Item {
            id: header
            width: parent.width
            implicitHeight: hero.implicitHeight
            // В trailingControl `root` — это PanelHero, а не панель: достаём её через header.
            readonly property var panel: root

          PanelHero {
            id: hero
            width: parent.width
            title: sync.status.name || "sharedir"
            meta: root.heroMeta
            foreground: root.foreground
            fontFamily: root.fontFamily
            iconOpacity: root.phase === "synced" || root.phase === "syncing" ? 1.0 : 0.5
            iconComponent: Component {
              Text {
                text: root.barGlyph
                color: root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.display
              }
            }
            trailingControl: Component {
              Row {
                spacing: Style.space(4)
                visible: sync.running

                PanelActionButton {
                  anchors.verticalCenter: parent.verticalCenter
                  iconText: "󰝰"  // md-folder_open
                  tooltipText: header.panel.tr("Open folder")
                  foreground: hero.foreground
                  fontFamily: hero.fontFamily
                  onClicked: sync.openFolder()
                }
                PanelActionButton {
                  anchors.verticalCenter: parent.verticalCenter
                  iconText: "󰆏"  // md-content_copy
                  tooltipText: header.panel.tr("Copy my ID")
                  foreground: hero.foreground
                  fontFamily: hero.fontFamily
                  onClicked: sync.copyId(S.prettyId(sync.status.id))
                }
                ToggleSwitch {
                  id: powerSwitch
                  anchors.verticalCenter: parent.verticalCenter
                  checked: sync.active
                  busy: sync.busy
                  foreground: hero.foreground
                  onToggled: sync.toggle()

                  PanelToolTip {
                    visible: powerSwitch.containsMouse
                    text: sync.active ? header.panel.tr("Pause syncing") : header.panel.tr("Resume syncing")
                    fontFamily: hero.fontFamily
                  }
                }
              }
            }
          }
          }

          Text {
            textFormat: Text.PlainText
            width: parent.width
            text: root.statusLine
            color: sync.lastError !== "" || sync.folderError || sync.scanError ? root.urgent : root.dim
            font.family: root.fontFamily
            font.pixelSize: Style.font.bodySmall
            wrapMode: Text.WordWrap
          }

          Column {
            visible: sync.folderError !== null || sync.lastResult !== ""
            width: parent.width
            spacing: Style.space(8)

            Text {
              width: parent.width
              textFormat: Text.PlainText
              wrapMode: Text.WordWrap
              color: root.foreground
              font.family: root.fontFamily
              font.pixelSize: Style.font.bodySmall
              text: sync.folderError ? (sync.folderError.code === "root_empty"
                ? root.tr("The folder is empty. Choose carefully.")
                : root.tr("Choose how to continue with this folder.")) : ""
            }

            Row {
              visible: sync.folderError !== null && root.folderConfirmAction === ""
              width: parent.width
              spacing: Style.space(8)
              Button {
                text: root.tr("Start over (reset)")
                bordered: true
                foreground: root.foreground
                fontFamily: root.fontFamily
                onClicked: { root.folderConfirmAction = "reset"; sync.lastError = ""; sync.lastResult = "" }
              }
              Button {
                text: root.tr("Keep this folder (rebind)")
                bordered: true
                foreground: root.foreground
                fontFamily: root.fontFamily
                onClicked: { root.folderConfirmAction = "rebind"; sync.lastError = ""; sync.lastResult = "" }
              }
            }

            Text {
              visible: sync.folderError !== null && root.folderConfirmAction !== ""
              width: parent.width
              textFormat: Text.PlainText
              wrapMode: Text.WordWrap
              color: root.urgent
              font.family: root.fontFamily
              font.pixelSize: Style.font.bodySmall
              text: root.folderConfirmAction === "reset"
                ? root.tr("Reset explanation")
                : (sync.folderError && sync.folderError.code === "root_empty"
                  ? root.tr("Rebind empty warning") : root.tr("Rebind explanation"))
            }

            Row {
              visible: sync.folderError !== null && root.folderConfirmAction !== ""
              width: parent.width
              spacing: Style.space(8)
              Button {
                text: root.tr("Confirm")
                bordered: true
                foreground: root.foreground
                fontFamily: root.fontFamily
                enabled: !sync.busy
                onClicked: sync.confirmFolder(root.folderConfirmAction)
              }
              Button {
                text: root.tr("Cancel")
                bordered: true
                foreground: root.foreground
                fontFamily: root.fontFamily
                onClicked: root.folderConfirmAction = ""
              }
            }

            Text {
              visible: sync.lastResult !== ""
              width: parent.width
              textFormat: Text.PlainText
              wrapMode: Text.WordWrap
              color: sync.lastResult === "folder-confirm-failed" || sync.lastResult === "folder-confirm-stale" ? root.urgent : root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.bodySmall
              text: sync.lastResult === "folder-confirm-stale" ? root.tr("Folder status changed. Wait for an update, then confirm again.")
                : sync.lastResult === "folder-reset-success" ? root.tr("Folder reset confirmed")
                : sync.lastResult === "folder-rebind-success" ? root.tr("Folder rebind confirmed")
                : root.tr("Confirmation failed; folder remains stopped")
            }
          }

          Button {
            visible: !sync.running
            text: root.tr("Start")
            bordered: true
            foreground: root.foreground
            fontFamily: root.fontFamily
            onClicked: sync.startDaemon()
          }

          // ButtonGroup-вкладки
          Row {
            visible: sync.running
            width: parent.width
            spacing: Style.space(8)

            Button {
              width: (parent.width - parent.spacing) / 2
              text: root.tr("Devices")
              selected: root.tab === "devices"
              bordered: true
              foreground: root.foreground
              fontFamily: root.fontFamily
              onClicked: root.tab = "devices"
            }
            Button {
              width: (parent.width - parent.spacing) / 2
              text: root.tr("Activity")
              selected: root.tab === "activity"
              bordered: true
              foreground: root.foreground
              fontFamily: root.fontFamily
              onClicked: root.tab = "activity"
            }
          }

          // ===== Устройства =====
          Column {
            visible: sync.running && root.tab === "devices"
            width: parent.width
            spacing: Style.space(12)

            PanelSeparator { visible: sync.peers.length > 0; foreground: root.foreground }
            SectionHeader { visible: sync.peers.length > 0; text: root.tr("DEVICES") }

            Repeater {
              model: sync.peers
              PeerItem {
                required property var modelData
                width: parent.width
                peer: modelData
              }
            }

            Column {
              visible: sync.candidates.length > 0
              width: parent.width
              spacing: Style.space(12)

              PanelSeparator { foreground: root.foreground }
              SectionHeader { text: root.tr("FOUND ON NETWORK") }

              Repeater {
                model: sync.candidates
                CandidateItem {
                  required property var modelData
                  width: parent.width
                  candidate: modelData
                }
              }
            }

            PanelSeparator { foreground: root.foreground }
            Item {
              width: parent.width
              implicitHeight: folderHeader.implicitHeight
              SectionHeader { id: folderHeader; text: root.tr("FOLDER") }
              Text {
                anchors.right: parent.right
                anchors.verticalCenter: folderHeader.verticalCenter
                textFormat: Text.PlainText
                text: S.files(root.lang, sync.status.files || 0) + " · " + S.bytes(sync.status.bytes)
                color: root.dim
                font.family: root.fontFamily
                font.pixelSize: Style.font.caption
              }
            }

            Repeater {
              model: sync.kinds.slice(0, 4)
              ShareBar {
                required property var modelData
                width: parent.width
                label: S.kind(root.lang, modelData.label)
                value: S.bytes(modelData.bytes)
                fraction: sync.status.bytes > 0 ? modelData.bytes / sync.status.bytes : 0
              }
            }
          }

          // ===== Активность =====
          Column {
            visible: sync.running && root.tab === "activity"
            width: parent.width
            spacing: Style.space(12)

            PanelSeparator { foreground: root.foreground }
            SectionHeader { text: root.tr("TRANSFERRED BY DAY") }

            Repeater {
              model: sync.days
              DayRow {
                required property var modelData
                required property int index
                width: parent.width
                day: modelData
                isToday: index === sync.days.length - 1
                maxBytes: {
                  var m = 0
                  for (var i = 0; i < sync.days.length; i++) m = Math.max(m, sync.days[i].in + sync.days[i].out)
                  return m
                }
              }
            }

            PanelSeparator { foreground: root.foreground }
            SectionHeader { text: root.tr("RECENT FILES") }

            Text {
              visible: sync.recent.length === 0
              width: parent.width
              textFormat: Text.PlainText
              text: root.tr("No changes yet")
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.body
              horizontalAlignment: Text.AlignHCenter
            }

            Column {
              width: parent.width
              spacing: Style.space(2)
              Repeater {
                model: sync.recent.slice(0, 8)
                EventRow {
                  required property var modelData
                  width: parent.width
                  event: modelData
                }
              }
            }
          }
        }
      }
    }
  }

  // ===== Компоненты =====

  component SectionHeader: PanelSectionHeader {
    foreground: root.foreground
    fontFamily: root.fontFamily
    font.letterSpacing: 1.2
  }

  component Caption: Text {
    textFormat: Text.PlainText
    color: root.dim
    font.family: root.fontFamily
    font.pixelSize: Style.font.caption
    elide: Text.ElideRight
  }

  component PeerItem: Column {
    property var peer: ({})
    spacing: Style.space(6)

    Item {
      width: parent.width
      implicitHeight: peerName.implicitHeight
      Text {
        id: peerName
        textFormat: Text.PlainText
        text: peer.name || ""
        color: root.foreground
        font.family: root.fontFamily
        font.pixelSize: Style.font.subtitle
        elide: Text.ElideRight
        width: parent.width - peerBadge.implicitWidth - Style.space(8)
      }
      Text {
        id: peerBadge
        anchors.right: parent.right
        anchors.baseline: peerName.baseline
        textFormat: Text.PlainText
        text: S.peerBadge(root.lang, peer)
        color: peer.connected ? root.foreground : root.dim
        font.family: root.fontFamily
        font.pixelSize: Style.font.body
      }
    }

    Meter {
      width: parent.width
      fraction: peer.progress && peer.progress.bytes > 0
        ? peer.progress.done_bytes / peer.progress.bytes
        : (peer.state === "synced" ? 1 : 0)
    }

    Item {
      width: parent.width
      implicitHeight: peerCaption.implicitHeight
      Caption {
        id: peerCaption
        width: parent.width - (peerLink.text !== "" ? peerLink.implicitWidth + Style.space(8) : 0)
        text: S.peerCaption(root.lang, peer)
      }
      // Тип связи: через relay медленнее, пусть это будет видно.
      Caption {
        id: peerLink
        anchors.right: parent.right
        anchors.baseline: peerCaption.baseline
        text: S.peerLink(root.lang, peer)
        color: peer.addr === "relay" ? root.foreground : root.dim
        elide: Text.ElideNone
      }
    }
  }

  component Meter: Rectangle {
    property real fraction: 0
    height: Style.space(4)
    color: Qt.rgba(root.foreground.r, root.foreground.g, root.foreground.b, 0.12)
    Rectangle {
      height: parent.height
      width: parent.width * Math.max(0, Math.min(1, parent.fraction))
      color: root.foreground
      Behavior on width { NumberAnimation { duration: 200; easing.type: Easing.OutCubic } }
    }
  }

  component CandidateItem: Column {
    id: cand
    property var candidate: ({})
    readonly property bool confirming: root.pairingId === candidate.id
    spacing: Style.space(8)

    RowLayout {
      width: parent.width
      spacing: Style.space(8)

      Text {
        text: "󰌢"  // md-laptop
        color: root.foreground
        font.family: root.fontFamily
        font.pixelSize: Style.font.icon
        Layout.alignment: Qt.AlignVCenter
      }
      ColumnLayout {
        Layout.fillWidth: true
        spacing: Style.space(1)
        Text {
          Layout.fillWidth: true
          textFormat: Text.PlainText
          text: cand.candidate.name || root.tr("Incoming connection")
          color: root.foreground
          font.family: root.fontFamily
          font.pixelSize: Style.font.body
          elide: Text.ElideRight
        }
        Caption {
          Layout.fillWidth: true
          text: (cand.candidate.addr ? cand.candidate.addr.replace(/:\d+$/, "") + " · " : "") + S.prettyId(cand.candidate.id).slice(0, 9)
        }
      }
      Button {
        visible: !cand.confirming
        text: root.tr("Add")
        bordered: true
        foreground: root.foreground
        fontFamily: root.fontFamily
        Layout.alignment: Qt.AlignVCenter
        onClicked: root.pairingId = cand.candidate.id
      }
    }

    Rectangle {
      visible: cand.confirming
      width: parent.width
      implicitHeight: confirmColumn.implicitHeight + Style.space(24)
      color: Qt.rgba(root.foreground.r, root.foreground.g, root.foreground.b, 0.04)
      border.width: 1
      border.color: Qt.rgba(root.foreground.r, root.foreground.g, root.foreground.b, 0.4)
      radius: Style.cornerRadius

      Column {
        id: confirmColumn
        anchors.fill: parent
        anchors.margins: Style.space(12)
        spacing: Style.space(8)

        Caption {
          width: parent.width
          font.pixelSize: Style.font.bodySmall
          wrapMode: Text.WordWrap
          text: root.tr("Compare the ID with the screen of %1:", [cand.candidate.name || root.tr("that device")])
        }
        Text {
          width: parent.width
          textFormat: Text.PlainText
          text: S.prettyId(cand.candidate.id)
          color: root.foreground
          font.family: root.fontFamily
          font.pixelSize: Style.font.body
          wrapMode: Text.WrapAnywhere
          lineHeight: 1.3
        }
        Row {
          anchors.right: parent.right
          spacing: Style.space(8)
          Button {
            text: root.tr("Cancel")
            bordered: true
            foreground: root.foreground
            fontFamily: root.fontFamily
            onClicked: root.pairingId = ""
          }
          Button {
            text: root.tr("Pair")
            selected: true
            foreground: root.foreground
            fontFamily: root.fontFamily
            onClicked: {
              sync.pair(cand.candidate.id)
              root.pairingId = ""
            }
          }
        }
      }
    }
  }

  // Строка-доля, как «Tokens by model» у Agents: заливка показывает долю.
  component ShareBar: Rectangle {
    property string label: ""
    property string value: ""
    property real fraction: 0
    implicitHeight: Style.spacing.popupRowHeight || Style.space(24)
    color: Qt.rgba(root.foreground.r, root.foreground.g, root.foreground.b, 0.04)

    Rectangle {
      anchors.left: parent.left
      anchors.top: parent.top
      anchors.bottom: parent.bottom
      width: parent.width * Math.max(0, Math.min(1, parent.fraction))
      color: Qt.rgba(root.foreground.r, root.foreground.g, root.foreground.b, 0.18)
    }
    Text {
      anchors.left: parent.left
      anchors.leftMargin: Style.space(8)
      anchors.verticalCenter: parent.verticalCenter
      textFormat: Text.PlainText
      text: parent.label
      color: root.foreground
      font.family: root.fontFamily
      font.pixelSize: Style.font.body
    }
    Text {
      anchors.right: parent.right
      anchors.rightMargin: Style.space(8)
      anchors.verticalCenter: parent.verticalCenter
      textFormat: Text.PlainText
      text: parent.value
      color: root.foreground
      font.family: root.fontFamily
      font.pixelSize: Style.font.body
    }
  }

  component DayRow: RowLayout {
    property var day: ({})
    property bool isToday: false
    property real maxBytes: 0
    readonly property real total: (day.in || 0) + (day.out || 0)
    spacing: Style.space(12)

    Text {
      Layout.preferredWidth: Style.space(58)
      textFormat: Text.PlainText
      text: S.dayLabel(root.lang, day.date, isToday)
      color: isToday ? root.foreground : root.dim
      font.family: root.fontFamily
      font.pixelSize: Style.font.body
      font.bold: isToday
    }
    Rectangle {
      Layout.fillWidth: true
      Layout.alignment: Qt.AlignVCenter
      Layout.preferredHeight: Style.space(4)
      color: Qt.rgba(root.foreground.r, root.foreground.g, root.foreground.b, 0.12)
      Rectangle {
        height: parent.height
        width: parent.width * (maxBytes > 0 ? total / maxBytes : 0)
        color: isToday ? root.foreground : Qt.rgba(root.foreground.r, root.foreground.g, root.foreground.b, 0.55)
      }
    }
    Text {
      Layout.preferredWidth: Style.space(46)
      horizontalAlignment: Text.AlignRight
      textFormat: Text.PlainText
      text: S.bytes(total)
      color: root.foreground
      font.family: root.fontFamily
      font.pixelSize: Style.font.body
      font.bold: isToday
    }
  }

  component EventRow: CursorSurface {
    id: eventRow
    property var event: ({})
    foreground: root.foreground
    hasCursor: rowMouse.containsMouse
    implicitHeight: eventContent.implicitHeight + Style.spacing.rowPaddingX

    readonly property string mark: {
      switch (event.kind) {
      case "in": return "󰁅"        // md-arrow_down
      case "out": return "󰁝"       // md-arrow_up
      case "conflict": return "󰀨"  // md-alert_circle
      default: return "󰆴"          // md-delete
      }
    }
    readonly property string meta: {
      var who = event.kind === "out" ? root.tr("to %1", [event.peer]) : root.tr("from %1", [event.peer])
      if (event.kind === "deleted") who = root.tr("deleted")
      if (event.kind === "conflict") who = root.tr("conflict")
      return who + " · " + S.clock(event.time) + (event.size ? " · " + S.bytes(event.size) : "")
    }

    MouseArea {
      id: rowMouse
      anchors.fill: parent
      hoverEnabled: true
      cursorShape: event.kind === "deleted" ? Qt.ArrowCursor : Qt.PointingHandCursor
      onClicked: if (event.kind !== "deleted") sync.revealFile(event.name)
    }

    RowLayout {
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.verticalCenter: parent.verticalCenter
      anchors.leftMargin: Style.space(10)
      anchors.rightMargin: Style.space(10)
      spacing: Style.space(8)

      Text {
        text: eventRow.mark
        color: event.kind === "conflict" ? root.urgent : (event.kind === "deleted" ? root.dim : root.foreground)
        font.family: root.fontFamily
        font.pixelSize: Style.font.icon
        Layout.alignment: Qt.AlignVCenter
      }
      ColumnLayout {
        id: eventContent
        Layout.fillWidth: true
        spacing: Style.space(1)
        Text {
          Layout.fillWidth: true
          textFormat: Text.PlainText
          text: event.name || ""
          color: root.foreground
          font.family: root.fontFamily
          font.pixelSize: Style.font.body
          elide: Text.ElideMiddle
        }
        Caption {
          Layout.fillWidth: true
          text: eventRow.meta
        }
      }
    }
  }
}
