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
                MenuDivider()
                folderRow
                MenuDivider()
                devices
                if !store.status.candidates.isEmpty {
                    MenuDivider()
                    candidates
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
        case .paused: return L("Syncing is paused")
        case .noPeers: return L("No paired devices")
        case .offline: return L("Offline — changes will sync later")
        case .syncing:
            if let p = st.peers.compactMap(\.progress).first {
                return L("Syncing · %@, %@", Format.files(p.files - p.doneFiles), Format.bytes(p.bytes - p.doneBytes))
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
    let peer: DaemonStatus.Peer

    var body: some View {
        HStack(spacing: 10) {
            DeviceBadge(symbol: "desktopcomputer", active: peer.connected && !peer.isPaused)
            VStack(alignment: .leading, spacing: 3) {
                Text(peer.name).font(.system(size: 13, weight: .medium))
                Text(subtitle).font(.system(size: 11)).foregroundStyle(secondaryColor).lineLimit(1)
                if let p = peer.progress, p.bytes > 0 {
                    ProgressView(value: Double(p.doneBytes), total: Double(p.bytes))
                        .progressViewStyle(.linear)
                        .controlSize(.mini)
                }
            }
            Spacer(minLength: 4)
            if peer.syncing {
                ProgressView().controlSize(.mini)
            } else if peer.connected && peer.isSynced {
                Image(systemName: "checkmark").font(.system(size: 11, weight: .semibold)).foregroundStyle(secondaryColor)
            }
        }
        .padding(.horizontal, 8).padding(.vertical, 4)
    }

    private var subtitle: String {
        // Связь через relay медленнее прямой — пусть это будет видно.
        guard peer.connected, peer.addr == "relay" else { return status }
        return L("%@ · via relay", status)
    }

    private var status: String {
        if let p = peer.progress {
            return L("Receiving %d of %d · %@ left", p.doneFiles, p.files, Format.bytes(p.bytes - p.doneBytes))
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
