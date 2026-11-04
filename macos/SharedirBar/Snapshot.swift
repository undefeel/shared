import AppKit
import SwiftUI

/// Отладочный рендер панели в PNG: <имя>.light.png и <имя>.dark.png.
enum Snapshot {
    @MainActor
    static func render(to url: URL) async {
        let store = Store()
        try? await Task.sleep(for: .seconds(2))
        await store.poll()
        let env = ProcessInfo.processInfo.environment
        // SHAREDIR_SNAPSHOT_STATUS=file.json — подставить статус (для вёрстки
        // состояний, которые трудно поймать вживую), SHAREDIR_SNAPSHOT_TAB=activity.
        if let path = env["SHAREDIR_SNAPSHOT_STATUS"] {
            for file in path.split(separator: ",").map(String.init) {
                if let data = FileManager.default.contents(atPath: file),
                   let st = try? StatusDecoder.shared.decode(DaemonStatus.self, from: data) {
                    store.inject(st)
                }
                try? await Task.sleep(for: .milliseconds(400))
            }
        }
        if let tab = env["SHAREDIR_SNAPSHOT_TAB"].flatMap(MenuTab.init(rawValue:)) { store.tab = tab }
        if ProcessInfo.processInfo.environment["SHAREDIR_SNAPSHOT_PAIR"] != nil {
            store.pairing = store.status.candidates.first?.id
        }

        // Темы рендерим по отдельности: смешанное дерево ImageRenderer
        // красит иерархические цвета по appearance приложения.
        for (name, appearance, scheme) in [("light", NSAppearance.Name.aqua, ColorScheme.light), ("dark", .darkAqua, .dark)] {
            NSApp.appearance = NSAppearance(named: appearance)
            let view = panel(store)
                .environment(\.colorScheme, scheme)
                .padding(24)
                .background(Color(red: 0.51, green: 0.59, blue: 0.67))
            let renderer = ImageRenderer(content: view)
            renderer.scale = 2
            let out = url.deletingPathExtension().appendingPathExtension("\(name).png")
            if let image = renderer.nsImage,
               let tiff = image.tiffRepresentation,
               let png = NSBitmapImageRep(data: tiff)?.representation(using: .png, properties: [:]) {
                try? png.write(to: out)
            }
        }
        exit(0)
    }

    @MainActor
    private static func panel(_ store: Store) -> some View {
        MenuContent()
            .environmentObject(store)
            .background(.regularMaterial)
            .background(Color(nsColor: .windowBackgroundColor))
            .clipShape(RoundedRectangle(cornerRadius: 14))
    }
}
