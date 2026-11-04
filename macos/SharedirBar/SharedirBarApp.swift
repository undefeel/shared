import AppKit
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
            MenuBarIcon(phase: store.phase, fraction: store.totalProgress?.fraction)
        }
        .menuBarExtraStyle(.window)
    }
}

/// Template-символ: macOS сама красит его под светлый/тёмный меню-бар,
/// состояние читается по форме.
struct MenuBarIcon: View {
    let phase: Phase
    var fraction: Double? = nil

    var body: some View {
        switch phase {
        case .synced:
            Image(systemName: "folder.fill")
        case .syncing:
            if let f = fraction {
                Image(nsImage: ProgressRing.image(f))
            } else {
                Image(systemName: "arrow.triangle.2.circlepath")
            }
        case .setup, .noPeers:
            Image(systemName: "folder.badge.plus")
        case .notRunning, .incompatible:
            Image(systemName: "exclamationmark.triangle")
        case .starting, .paused, .offline:
            Image(systemName: "folder")
        }
    }
}

/// Кольцо прогресса со стрелкой для меню-бара. Template-картинка:
/// цвет даёт macOS, слабый трек — через прозрачность. Шаг 5 %, картинки кэшируются.
@MainActor
enum ProgressRing {
    private static var cache: [Int: NSImage] = [:]

    static func image(_ fraction: Double) -> NSImage {
        let step = Int((min(1, max(0, fraction)) * 20).rounded())
        if let img = cache[step] { return img }
        let f = CGFloat(step) / 20
        let img = NSImage(size: NSSize(width: 18, height: 18), flipped: false) { r in
            let rect = r.insetBy(dx: 1.6, dy: 1.6)
            let center = NSPoint(x: r.midX, y: r.midY)
            let track = NSBezierPath(ovalIn: rect)
            track.lineWidth = 1.6
            NSColor.black.withAlphaComponent(0.3).setStroke()
            track.stroke()
            if f > 0 {
                let arc = NSBezierPath()
                arc.appendArc(withCenter: center, radius: rect.width / 2, startAngle: 90, endAngle: 90 - 360 * f, clockwise: true)
                arc.lineWidth = 1.6
                arc.lineCapStyle = .round
                NSColor.black.setStroke()
                arc.stroke()
            }
            if let arrow = NSImage(systemSymbolName: "arrow.down", accessibilityDescription: nil)?
                .withSymbolConfiguration(.init(pointSize: 8, weight: .bold)) {
                let s = arrow.size
                arrow.draw(in: NSRect(x: center.x - s.width / 2, y: center.y - s.height / 2, width: s.width, height: s.height))
            }
            return true
        }
        img.isTemplate = true
        img.accessibilityDescription = "sharedir"
        cache[step] = img
        return img
    }
}
