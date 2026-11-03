// Генератор macos/Resources/AppIcon.icns:
//   swiftc macos/tools/make-icon.swift -o /tmp/mkicon && /tmp/mkicon /tmp/AppIcon.iconset
//   iconutil -c icns /tmp/AppIcon.iconset -o macos/Resources/AppIcon.icns
import AppKit

// Иконка: скруглённый квадрат системного синего с белой папкой и стрелками синхронизации.
func render(_ px: Int) -> Data {
    let size = CGFloat(px)
    let img = NSImage(size: NSSize(width: size, height: size))
    img.lockFocus()
    let inset = size * 0.1
    let rect = NSRect(x: inset, y: inset, width: size - 2 * inset, height: size - 2 * inset)
    let path = NSBezierPath(roundedRect: rect, xRadius: rect.width * 0.225, yRadius: rect.width * 0.225)
    NSGradient(starting: NSColor(srgbRed: 0.24, green: 0.58, blue: 1.0, alpha: 1),
               ending: NSColor(srgbRed: 0.0, green: 0.42, blue: 0.95, alpha: 1))!.draw(in: path, angle: -90)
    func symbol(_ name: String, _ pt: CGFloat, _ color: NSColor) -> NSImage? {
        NSImage(systemSymbolName: name, accessibilityDescription: nil)?
            .withSymbolConfiguration(NSImage.SymbolConfiguration(pointSize: pt, weight: .semibold)
                .applying(.init(paletteColors: [color])))
    }
    func drawCentered(_ img: NSImage, dy: CGFloat = 0) {
        let s = img.size
        img.draw(in: NSRect(x: (size - s.width) / 2, y: (size - s.height) / 2 + dy, width: s.width, height: s.height))
    }
    if let folder = symbol("folder.fill", size * 0.40, .white) { drawCentered(folder) }
    if let arrows = symbol("arrow.triangle.2.circlepath", size * 0.17, NSColor(srgbRed: 0.1, green: 0.5, blue: 0.98, alpha: 1)) {
        drawCentered(arrows, dy: -size * 0.03)
    }
    img.unlockFocus()
    let rep = NSBitmapImageRep(data: img.tiffRepresentation!)!
    return rep.representation(using: .png, properties: [:])!
}

let out = URL(fileURLWithPath: CommandLine.arguments[1])
try? FileManager.default.createDirectory(at: out, withIntermediateDirectories: true)
for base in [16, 32, 128, 256, 512] {
    try! render(base).write(to: out.appendingPathComponent("icon_\(base)x\(base).png"))
    try! render(base * 2).write(to: out.appendingPathComponent("icon_\(base)x\(base)@2x.png"))
}
