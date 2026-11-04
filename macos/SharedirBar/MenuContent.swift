import Charts
import SwiftUI

/// Цвета текста системных меню. Явные NSColor, а не .primary/.secondary:
/// иерархические стили в окне MenuBarExtra и в ImageRenderer иногда
/// резолвятся по appearance приложения, а не окна.
private let labelColor = Color(nsColor: .labelColor)
private let secondaryColor = Color(nsColor: .secondaryLabelColor)

/// Содержимое окна MenuBarExtra в стиле системных меню (Wi-Fi, Bluetooth).
struct MenuContent: View {
    @EnvironmentObject var store: Store

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            header
            switch store.phase {
            case .setup:
                MenuDivider()
                SetupView()
            case .notRunning, .starting:
                MenuDivider()
                NotRunningView()
            default:
                if let total = store.totalProgress {
                    ProgressView(value: total.fraction)
                        .progressViewStyle(MeterStyle(active: !store.status.paused, height: 5))
                        .padding(.horizontal, 8).padding(.top, 2).padding(.bottom, 4)
                }
                Picker(L("View"), selection: $store.tab) {
                    Text(L("Devices")).tag(MenuTab.devices)
                    Text(L("Activity")).tag(MenuTab.activity)
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .controlSize(.small)
                .padding(.horizontal, 8).padding(.vertical, 4)
                MenuDivider()
                switch store.tab {
                case .devices:
                    folderRow
                    MenuDivider()
                    devices
                    if !store.status.candidates.isEmpty {
                        MenuDivider()
                        candidates
                    }
                    if store.status.bytes > 0 {
                        MenuDivider()
                        FolderBreakdown()
                    }
                case .activity:
                    ActivityView()
                }
            }
            if let err = store.lastError {
                Text(err)
                    .font(.system(size: 11))
                    .foregroundStyle(.red)
                    .padding(.horizontal, 8)
                    .padding(.vertical, 4)
                    .fixedSize(horizontal: false, vertical: true)
            }
            MenuDivider()
            footer
        }
        .foregroundStyle(labelColor)
        .padding(6)
        .frame(width: 320)
    }

    // MARK: Заголовок

    private var header: some View {
        HStack(alignment: .center, spacing: 10) {
            VStack(alignment: .leading, spacing: 2) {
                Text(verbatim: "sharedir").font(.system(size: 13, weight: .bold))
                Text(headline).font(.system(size: 11)).foregroundStyle(secondaryColor)
            }
            Spacer()
            if store.status.running {
                Toggle(L("Sync"), isOn: Binding(
                    get: { !store.status.paused },
                    set: { store.setPaused(!$0) }
                ))
                .toggleStyle(.switch)
                .controlSize(.mini)
                .labelsHidden()
                .help(store.status.paused ? L("Resume syncing") : L("Pause syncing"))
            }
        }
        .padding(.horizontal, 8)
        .padding(.top, 6)
        .padding(.bottom, 4)
    }

    private var headline: String {
        let st = store.status
        switch store.phase {
        case .setup: return L("Setup needed")
        case .starting: return L("Starting…")
        case .notRunning: return L("Daemon isn’t running")
        case .paused:
            if let p = store.totalProgress {
                return L("Paused · %@ received", Format.percent(p.fraction))
            }
            return L("Syncing is paused")
        case .noPeers: return L("No paired devices")
        case .offline: return L("Offline — changes will sync later")
        case .syncing:
            if let p = store.totalProgress {
                return L("Syncing · %@ · %@ left", Format.percent(p.fraction), Format.bytes(p.leftBytes))
            }
            return L("Syncing…")
        case .synced:
            if let last = st.peers.compactMap(\.lastSync).max() {
                return L("Up to date · %@", Format.time(last))
            }
            return L("Up to date")
        }
    }

    // MARK: Папка

    private var folderRow: some View {
        MenuRow(action: store.openFolder) {
            DeviceBadge(symbol: "folder.fill", active: true)
            VStack(alignment: .leading, spacing: 1) {
                Text(URL(fileURLWithPath: store.status.folder).lastPathComponent)
                    .font(.system(size: 13, weight: .medium))
                Text(verbatim: "\(Format.abbreviatedPath(store.status.folder)) · \(Format.files(store.status.files)) · \(Format.bytes(store.status.bytes))")
                    .font(.system(size: 11)).foregroundStyle(secondaryColor)
                    .lineLimit(1).truncationMode(.middle)
            }
            Spacer(minLength: 4)
            Text(L("Open")).font(.system(size: 11)).foregroundStyle(secondaryColor)
            Image(systemName: "chevron.right").font(.system(size: 10, weight: .semibold)).foregroundStyle(secondaryColor)
        }
    }

    // MARK: Устройства

    private var devices: some View {
        VStack(alignment: .leading, spacing: 0) {
            SectionTitle(L("Devices"))
            if store.status.peers.isEmpty {
                Text(L("None yet. Add a device below, then do the same on it."))
                    .font(.system(size: 12)).foregroundStyle(secondaryColor)
                    .padding(.horizontal, 8).padding(.bottom, 6)
                    .fixedSize(horizontal: false, vertical: true)
            }
            ForEach(store.status.peers) { peer in
                PeerRow(peer: peer)
            }
        }
    }

    // MARK: Найдено в сети

    private var candidates: some View {
        VStack(alignment: .leading, spacing: 0) {
            SectionTitle(L("Found on This Network"))
            ForEach(store.status.candidates) { c in
                HStack(spacing: 10) {
                    DeviceBadge(symbol: "laptopcomputer", active: false)
                    VStack(alignment: .leading, spacing: 1) {
                        Text(c.name ?? L("Incoming connection")).font(.system(size: 13, weight: .medium))
                        Text(c.addr ?? Format.prettyID(c.id).prefix(9) + "…")
                            .font(.system(size: 11)).foregroundStyle(secondaryColor)
                    }
                    Spacer()
                    if store.pairing != c.id {
                        Button(L("Add…")) { withAnimation(.snappy) { store.pairing = c.id } }
                            .controlSize(.small)
                    }
                }
                .padding(.horizontal, 8).padding(.vertical, 4)

                if store.pairing == c.id {
                    PairConfirm(candidate: c) {
                        store.pair(c.id)
                    } onCancel: {
                        withAnimation(.snappy) { store.pairing = nil }
                    }
                }
            }
        }
    }

    // MARK: Низ

    private var footer: some View {
        VStack(alignment: .leading, spacing: 0) {
            if !store.status.id.isEmpty {
                MenuRow(action: store.copyID) {
                    Text(L("Copy My ID"))
                    Spacer()
                    Text(Format.prettyID(store.status.id).prefix(9) + "…")
                        .font(.system(size: 11, design: .monospaced)).foregroundStyle(secondaryColor)
                }
            }
            HStack {
                Text(L("Open at Login"))
                Spacer()
                Toggle(L("Open at Login"), isOn: Binding(
                    get: { store.launchAtLogin },
                    set: { store.setLaunchAtLogin($0) }
                ))
                .toggleStyle(.switch).controlSize(.mini).labelsHidden()
            }
            .font(.system(size: 13))
            .padding(.horizontal, 8).padding(.vertical, 4)
            MenuDivider()
            MenuRow(action: store.openLog) { Text(L("Show Log…")); Spacer() }
            MenuRow(action: { NSApp.terminate(nil) }) {
                Text(L("Quit sharedir"))
                Spacer()
                Text(verbatim: "⌘Q").foregroundStyle(secondaryColor)
            }
            .keyboardShortcut("q")
        }
    }
}

// MARK: - Строки

private struct PeerRow: View {
    @EnvironmentObject var store: Store
    let peer: DaemonStatus.Peer

    var body: some View {
        let progress = store.progress(for: peer)
        HStack(alignment: .top, spacing: 10) {
            DeviceBadge(symbol: "desktopcomputer", active: peer.connected && !peer.isPaused)
                .padding(.top, 1)
            VStack(alignment: .leading, spacing: 3) {
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    Text(peer.name).font(.system(size: 13, weight: .medium)).lineLimit(1)
                    Spacer(minLength: 4)
                    trailing(progress)
                }
                Text(subtitle(progress)).font(.system(size: 11)).foregroundStyle(secondaryColor).lineLimit(1)
                // Полоска есть всегда, как в Omarchy: синяя — идёт приём,
                // серая полная — всё актуально, серая частичная — пауза, пустая — нет связи.
                ProgressView(value: progress?.fraction ?? (peer.connected && peer.isSynced ? 1 : 0))
                    .progressViewStyle(MeterStyle(active: peer.progress != nil, height: 4))
                    .padding(.top, 1)
            }
        }
        .padding(.horizontal, 8).padding(.vertical, 4)
    }

    @ViewBuilder
    private func trailing(_ progress: DaemonStatus.Progress?) -> some View {
        if let p = peer.progress {
            Text(Format.percent(p.fraction))
                .font(.system(size: 12, weight: .medium)).monospacedDigit()
        } else if peer.syncing {
            ProgressView().controlSize(.mini)
        } else if peer.connected && peer.isSynced {
            Image(systemName: "checkmark").font(.system(size: 11, weight: .semibold)).foregroundStyle(secondaryColor)
        }
    }

    private func subtitle(_ progress: DaemonStatus.Progress?) -> String {
        let text = status(progress)
        // Связь через relay медленнее прямой — пусть это будет видно.
        guard peer.connected, peer.addr == "relay" else { return text }
        return L("%@ · via relay", text)
    }

    private func status(_ progress: DaemonStatus.Progress?) -> String {
        if let p = peer.progress {
            var text = L("Receiving %d of %d · %@ left", p.doneFiles, p.files, Format.bytes(p.leftBytes))
            if let v = store.speeds[peer.id], v >= 1000 { text += " · " + Format.speed(v) }
            return text
        }
        if peer.isPaused, let p = progress {
            return L("Paused · %@, %@ left", Format.files(p.leftFiles), Format.bytes(p.leftBytes))
        }
        switch peer.state {
        case "synced":
            return peer.lastSync.map { L("Up to date · %@", Format.ago($0)) } ?? L("Up to date")
        case "offline", "not_found":
            return peer.lastSync.map { L("Offline · last seen %@", Format.ago($0)) } ?? L("Offline")
        case "searching": return L("Looking for device…")
        case "syncing": return L("Syncing…")
        case "retry": return L("%d files failed, retrying", peer.failed ?? 0)
        case "paused": return L("Paused")
        case "peer_paused": return L("Paused on that device")
        default: return peer.state
        }
    }
}

// MARK: - Состав папки

private struct FolderBreakdown: View {
    @EnvironmentObject var store: Store

    /// Четыре крупных типа, остальное — в «Другое».
    private var slices: [DaemonStatus.Kind] {
        var top: [DaemonStatus.Kind] = []
        var other: Int64 = 0
        for k in store.status.kinds where k.bytes > 0 {
            if k.label != "other" && top.count < 4 { top.append(k) } else { other += k.bytes }
        }
        if other > 0 { top.append(.init(label: "other", bytes: other)) }
        return top
    }

    var body: some View {
        let slices = self.slices
        let total = max(1, slices.reduce(Int64(0)) { $0 + $1.bytes })
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                SectionTitle(L("Folder Contents"))
                Spacer()
                Text(verbatim: "\(Format.files(store.status.files)) · \(Format.bytes(store.status.bytes))")
                    .font(.system(size: 11)).monospacedDigit().foregroundStyle(secondaryColor)
                    .padding(.horizontal, 8).padding(.top, 3)
            }
            GeometryReader { g in
                let gaps = CGFloat(max(0, slices.count - 1)) * 1.5
                HStack(spacing: 1.5) {
                    ForEach(slices) { k in
                        Rectangle().fill(kindColor(k.label))
                            .frame(width: max(2, (g.size.width - gaps) * CGFloat(k.bytes) / CGFloat(total)))
                    }
                }
            }
            .frame(height: 8)
            .clipShape(Capsule())
            .padding(.horizontal, 8).padding(.top, 2).padding(.bottom, 8)

            LazyVGrid(columns: [GridItem(.flexible(), spacing: 12), GridItem(.flexible())], alignment: .leading, spacing: 4) {
                ForEach(slices) { k in
                    HStack(spacing: 5) {
                        Circle().fill(kindColor(k.label)).frame(width: 7, height: 7)
                        Text(Format.kind(k.label)).lineLimit(1).truncationMode(.tail).layoutPriority(1)
                        Spacer(minLength: 2)
                        Text(Format.bytes(k.bytes)).monospacedDigit().foregroundStyle(secondaryColor)
                            .lineLimit(1).fixedSize()
                    }
                    .font(.system(size: 11))
                }
            }
            .padding(.horizontal, 8).padding(.bottom, 6)
        }
    }
}

private func kindColor(_ code: String) -> Color {
    switch code {
    case "video": return Color(nsColor: .systemRed)
    case "images": return Color(nsColor: .systemOrange)
    case "docs": return Color(nsColor: .systemBlue)
    case "audio": return Color(nsColor: .systemPink)
    case "archive": return Color(nsColor: .systemBrown)
    case "code": return Color(nsColor: .systemGreen)
    default: return Color(nsColor: .systemGray)
    }
}

// MARK: - Активность

/// Выбранный на графике день (через @StateObject: @State без Xcode не собрать).
final class ChartSelection: ObservableObject {
    @Published var day: String?
}

private struct ActivityView: View {
    @EnvironmentObject var store: Store
    @StateObject private var selection = ChartSelection()

    var body: some View {
        let days = store.status.days
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                SectionTitle(L("Transferred This Week"))
                Spacer()
                if let today = days.last, today.total > 0 {
                    Text(L("today %@", Format.bytes(today.total)))
                        .font(.system(size: 11)).monospacedDigit().foregroundStyle(secondaryColor)
                        .padding(.horizontal, 8).padding(.top, 3)
                }
            }
            if days.contains(where: { $0.total > 0 }) {
                chart(days)
                Text(summary(days))
                    .font(.system(size: 11)).monospacedDigit().foregroundStyle(secondaryColor)
                    .padding(.horizontal, 8).padding(.top, 4).padding(.bottom, 4)
            } else {
                Text(L("Nothing transferred this week"))
                    .font(.system(size: 12)).foregroundStyle(secondaryColor)
                    .padding(.horizontal, 8).padding(.bottom, 6)
            }

            MenuDivider()
            SectionTitle(L("Recent Files"))
            if store.status.recent.isEmpty {
                Text(L("No changes yet"))
                    .font(.system(size: 12)).foregroundStyle(secondaryColor)
                    .padding(.horizontal, 8).padding(.bottom, 6)
            }
            ForEach(store.status.recent.prefix(6)) { ev in
                EventRow(event: ev)
            }
        }
    }

    private func chart(_ days: [DaemonStatus.Day]) -> some View {
        let received = L("Received"), sent = L("Sent")
        return Chart {
            ForEach(days) { d in
                BarMark(x: .value("Day", d.date), y: .value("Bytes", d.received))
                    .foregroundStyle(by: .value("Direction", received))
                BarMark(x: .value("Day", d.date), y: .value("Bytes", d.sent))
                    .foregroundStyle(by: .value("Direction", sent))
            }
            if let day = selection.day {
                RuleMark(x: .value("Day", day))
                    .foregroundStyle(Color(nsColor: .tertiaryLabelColor))
                    .lineStyle(StrokeStyle(lineWidth: 1))
            }
        }
        .chartForegroundStyleScale([received: Color.accentColor, sent: Color.accentColor.opacity(0.4)])
        .chartLegend(.hidden)
        .chartYAxis {
            AxisMarks(values: .automatic(desiredCount: 2)) { _ in
                AxisGridLine(stroke: StrokeStyle(lineWidth: 0.5, dash: [2, 3]))
            }
        }
        .chartXAxis {
            AxisMarks { value in
                AxisValueLabel {
                    if let date = value.as(String.self) {
                        Text(Format.weekday(date))
                            .font(.system(size: 10, weight: date == days.last?.date ? .semibold : .regular))
                            .foregroundStyle(date == days.last?.date ? labelColor : secondaryColor)
                    }
                }
            }
        }
        .chartXSelection(value: $selection.day)
        .frame(height: 96)
        .padding(.horizontal, 8).padding(.top, 2)
    }

    /// Под графиком: итог недели или выбранный день.
    private func summary(_ days: [DaemonStatus.Day]) -> String {
        if let sel = selection.day, let d = days.first(where: { $0.date == sel }) {
            return L("%@: received %@ · sent %@", Format.weekday(d.date), Format.bytes(d.received), Format.bytes(d.sent))
        }
        let r = days.reduce(Int64(0)) { $0 + $1.received }, s = days.reduce(Int64(0)) { $0 + $1.sent }
        return L("Received %@ · sent %@", Format.bytes(r), Format.bytes(s))
    }
}

private struct EventRow: View {
    @EnvironmentObject var store: Store
    let event: DaemonStatus.Event

    var body: some View {
        MenuRow(action: { if event.kind != "deleted" { store.reveal(event.name) } }) {
            Image(systemName: symbol)
                .font(.system(size: 15))
                .foregroundStyle(tint)
                .frame(width: 20)
            VStack(alignment: .leading, spacing: 1) {
                Text(event.name).font(.system(size: 12.5)).lineLimit(1).truncationMode(.middle)
                Text(meta).font(.system(size: 11)).foregroundStyle(secondaryColor).lineLimit(1)
            }
            Spacer(minLength: 0)
        }
    }

    private var symbol: String {
        switch event.kind {
        case "in": return "arrow.down.circle.fill"
        case "out": return "arrow.up.circle.fill"
        case "conflict": return "exclamationmark.triangle.fill"
        default: return "trash.circle"
        }
    }

    private var tint: Color {
        switch event.kind {
        case "in": return .accentColor
        case "out": return Color(nsColor: .systemGreen)
        case "conflict": return Color(nsColor: .systemOrange)
        default: return secondaryColor
        }
    }

    private var meta: String {
        var parts: [String]
        switch event.kind {
        case "in": parts = [L("from %@", event.peer)]
        case "out": parts = [L("to %@", event.peer)]
        case "conflict": parts = [L("conflict, both versions kept")]
        default: parts = [L("deleted")]
        }
        parts.append(Format.eventTime(event.time))
        if let size = event.size, size > 0, event.kind == "in" || event.kind == "out" { parts.append(Format.bytes(size)) }
        return parts.joined(separator: " · ")
    }
}

private struct PairConfirm: View {
    let candidate: DaemonStatus.Candidate
    let onConfirm: () -> Void
    let onCancel: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(candidate.name.map { L("Make sure “%@” shows the same ID:", $0) } ?? L("Make sure the other device shows the same ID:"))
                .font(.system(size: 12)).foregroundStyle(secondaryColor)
                .fixedSize(horizontal: false, vertical: true)
            Text(Format.prettyID(candidate.id))
                .font(.system(size: 11.5, design: .monospaced))
                .textSelection(.enabled)
                .fixedSize(horizontal: false, vertical: true)
            HStack {
                Spacer()
                Button(L("Cancel"), action: onCancel).controlSize(.small)
                Button(L("Pair"), action: onConfirm).controlSize(.small).buttonStyle(.borderedProminent)
            }
        }
        .padding(10)
        .background(RoundedRectangle(cornerRadius: 9).fill(Color.primary.opacity(0.06)))
        .padding(.horizontal, 8).padding(.bottom, 6)
    }
}

private struct SetupView: View {
    @EnvironmentObject var store: Store

    var body: some View {
        VStack(spacing: 8) {
            Image(systemName: "folder.fill")
                .font(.system(size: 22))
                .foregroundStyle(.white)
                .frame(width: 44, height: 44)
                .background(RoundedRectangle(cornerRadius: 12).fill(Color.accentColor))
            Text(L("No folder chosen yet")).font(.system(size: 13, weight: .semibold))
            Text(L("Its contents will sync with every paired device on your network."))
                .font(.system(size: 12)).foregroundStyle(secondaryColor)
                .multilineTextAlignment(.center)
                .fixedSize(horizontal: false, vertical: true)
            Button(L("Choose Folder…"), action: store.chooseFolder)
                .buttonStyle(.borderedProminent)
                .padding(.top, 4)
        }
        .frame(maxWidth: .infinity)
        .padding(.horizontal, 16).padding(.vertical, 16)
    }
}

private struct NotRunningView: View {
    @EnvironmentObject var store: Store

    var body: some View {
        HStack(spacing: 10) {
            DeviceBadge(symbol: "exclamationmark", active: false)
            VStack(alignment: .leading, spacing: 1) {
                Text(store.phase == .starting ? L("Starting the daemon…") : L("The daemon is not responding"))
                    .font(.system(size: 13, weight: .medium))
                Text(L("Details are in the log")).font(.system(size: 11)).foregroundStyle(secondaryColor)
            }
            Spacer()
            if store.phase == .notRunning {
                Button(L("Start")) { store.startDaemonIfNeeded(force: true) }.controlSize(.small)
            } else {
                ProgressView().controlSize(.mini)
            }
        }
        .padding(.horizontal, 8).padding(.vertical, 4)
    }
}

// MARK: - Мелкие компоненты

/// Полоска прогресса в духе NSProgressIndicator, но целиком на SwiftUI:
/// цвет задаём сами (акцент — идёт передача, серый — стоит), и она
/// рисуется в ImageRenderer для снимков.
struct MeterStyle: ProgressViewStyle {
    var active: Bool
    var height: CGFloat

    func makeBody(configuration: Configuration) -> some View {
        let f = min(1, max(0, configuration.fractionCompleted ?? 0))
        return GeometryReader { g in
            ZStack(alignment: .leading) {
                Capsule().fill(Color(nsColor: .labelColor).opacity(0.1))
                Capsule()
                    .fill(active ? Color.accentColor : Color(nsColor: .tertiaryLabelColor))
                    .frame(width: f > 0 ? max(height, g.size.width * f) : 0)
            }
        }
        .frame(height: height)
        .animation(.easeOut(duration: 0.3), value: f)
    }
}

/// Кружок с символом, как у устройств в меню Wi-Fi/Bluetooth.
struct DeviceBadge: View {
    let symbol: String
    let active: Bool

    var body: some View {
        Image(systemName: symbol)
            .font(.system(size: 12, weight: .medium))
            .foregroundStyle(active ? Color.white : labelColor.opacity(0.75))
            .frame(width: 26, height: 26)
            .background(Circle().fill(active ? Color.accentColor : Color(nsColor: .quaternaryLabelColor)))
    }
}

/// Строка-кнопка с подсветкой при наведении, как пункт системного меню.
struct MenuRow<Content: View>: View {
    let action: () -> Void
    @ViewBuilder let content: Content
    // @State в SDK — макрос, а его плагина нет в Command Line Tools;
    // @StateObject — обычная обёртка и собирается без Xcode.
    @StateObject private var hover = HoverFlag()

    var body: some View {
        Button(action: action) {
            HStack(spacing: 10) { content }
                .font(.system(size: 13))
                .foregroundStyle(labelColor)
                .padding(.horizontal, 8).padding(.vertical, 5)
                .frame(maxWidth: .infinity, alignment: .leading)
                .contentShape(Rectangle())
                .background(RoundedRectangle(cornerRadius: 7).fill(hover.on ? Color.primary.opacity(0.08) : .clear))
        }
        .buttonStyle(.plain)
        .onHover { hover.on = $0 }
    }
}

final class HoverFlag: ObservableObject {
    @Published var on = false
}

struct SectionTitle: View {
    let text: String
    init(_ text: String) { self.text = text }

    var body: some View {
        Text(text)
            .font(.system(size: 11, weight: .semibold))
            .foregroundStyle(secondaryColor)
            .padding(.horizontal, 8).padding(.top, 6).padding(.bottom, 3)
    }
}

struct MenuDivider: View {
    var body: some View {
        Divider().padding(.horizontal, 8).padding(.vertical, 4)
    }
}
