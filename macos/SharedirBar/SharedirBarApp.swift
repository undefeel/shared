import SwiftUI

@main
struct SharedirBarApp: App {
    @StateObject private var store = Store()

    init() {
        // SHAREDIR_SNAPSHOT=/path.png — отрисовать панель с живыми данными
        // демона в картинку и выйти (для проверки вёрстки без клика по меню).
        if let out = ProcessInfo.processInfo.environment["SHAREDIR_SNAPSHOT"] {
            Task { @MainActor in await Snapshot.render(to: URL(fileURLWithPath: out)) }
        }
    }

    var body: some Scene {
        MenuBarExtra {
            MenuContent().environmentObject(store)
        } label: {
            MenuBarIcon(phase: store.phase)
        }
        .menuBarExtraStyle(.window)
    }
}

/// Template-символ: macOS сама красит его под светлый/тёмный меню-бар,
/// состояние читается по форме.
struct MenuBarIcon: View {
    let phase: Phase

    var body: some View {
        switch phase {
        case .synced:
            Image(systemName: "folder.fill")
        case .syncing:
            Image(systemName: "arrow.triangle.2.circlepath")
        case .setup, .noPeers:
            Image(systemName: "folder.badge.plus")
        case .notRunning:
            Image(systemName: "exclamationmark.triangle")
        case .starting, .paused, .offline:
            Image(systemName: "folder")
        }
    }
}
