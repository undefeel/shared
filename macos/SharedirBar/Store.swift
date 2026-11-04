import AppKit
import Foundation
import ServiceManagement

// MARK: - Статус демона (зеркало app.Status)

struct DaemonStatus: Decodable, Equatable {
    var running: Bool
    var id: String = ""
    var name: String = ""
    var folder: String = ""
    var files: Int = 0
    var bytes: Int64 = 0
    var paused: Bool = false
    var syncing: Bool = false
    var peers: [Peer] = []
    var candidates: [Candidate] = []
    var kinds: [Kind] = []
    var days: [Day] = []
    var recent: [Event] = []
    var folderError: FolderError?
    var scanError: ScanError?

    struct FolderError: Decodable, Equatable {
        var code: String
        var message: String
        var token: String
    }

    struct ScanError: Decodable, Equatable {
        var message: String
        var path: String?
        var time: Date?
    }

    struct Peer: Decodable, Equatable, Identifiable {
        var id: String
        var name: String
        var connected: Bool
        var syncing: Bool
        var addr: String?
        var state: String
        var failed: Int?
        var lastSync: Date?
        var progress: Progress?
    }

    struct Progress: Decodable, Equatable {
        var files: Int
        var doneFiles: Int
        var bytes: Int64
        var doneBytes: Int64
    }

    struct Candidate: Decodable, Equatable, Identifiable {
        var id: String
        var name: String?
        var addr: String?
        var source: String
    }

    /// Объём папки по типу файлов; label — код (video, images, docs, audio, archive, code, other).
    struct Kind: Decodable, Equatable, Identifiable {
        var label: String
        var bytes: Int64
        var id: String { label }
    }

    /// Передано за день; date — «2006-01-02» по локальному времени демона.
    struct Day: Decodable, Equatable, Identifiable {
        var date: String
        var received: Int64
        var sent: Int64
        var id: String { date }
        var total: Int64 { received + sent }

        enum CodingKeys: String, CodingKey { case date, received = "in", sent = "out" }
    }

    /// Событие ленты: kind — in, out, deleted, conflict.
    struct Event: Decodable, Equatable, Identifiable {
        var time: Date
        var kind: String
        var name: String
        var peer: String
        var size: Int64?
        var id: String { "\(time.timeIntervalSince1970)|\(kind)|\(name)" }
    }

    enum CodingKeys: String, CodingKey {
        case running, id, name, folder, files, bytes, paused, syncing, peers, candidates, kinds, days, recent
        case folderError, scanError
    }

    init(running: Bool) { self.running = running }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        running = try c.decode(Bool.self, forKey: .running)
        id = try c.decodeIfPresent(String.self, forKey: .id) ?? ""
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? ""
        folder = try c.decodeIfPresent(String.self, forKey: .folder) ?? ""
        files = try c.decodeIfPresent(Int.self, forKey: .files) ?? 0
        bytes = try c.decodeIfPresent(Int64.self, forKey: .bytes) ?? 0
        paused = try c.decodeIfPresent(Bool.self, forKey: .paused) ?? false
        syncing = try c.decodeIfPresent(Bool.self, forKey: .syncing) ?? false
        peers = try c.decodeIfPresent([Peer].self, forKey: .peers) ?? []
        candidates = try c.decodeIfPresent([Candidate].self, forKey: .candidates) ?? []
        kinds = try c.decodeIfPresent([Kind].self, forKey: .kinds) ?? []
        days = try c.decodeIfPresent([Day].self, forKey: .days) ?? []
        recent = try c.decodeIfPresent([Event].self, forKey: .recent) ?? []
        folderError = try c.decodeIfPresent(FolderError.self, forKey: .folderError)
        scanError = try c.decodeIfPresent(ScanError.self, forKey: .scanError)
    }
}

extension DaemonStatus.Peer {
    /// Коды состояний демона (app.State*).
    var isPaused: Bool { state == "paused" || state == "peer_paused" }
    var isSynced: Bool { state == "synced" }
}

extension DaemonStatus.Progress {
    var fraction: Double { bytes > 0 ? min(1, Double(doneBytes) / Double(bytes)) : 0 }
    var leftBytes: Int64 { max(0, bytes - doneBytes) }
    var leftFiles: Int { max(0, files - doneFiles) }
}

/// Вкладки меню.
enum MenuTab: String {
    case devices, activity
}

/// Итоговое состояние для иконки и заголовка.
enum Phase: Equatable {
    case setup        // конфига нет — нужно выбрать папку
    case starting     // демон запускается
    case notRunning   // демон не отвечает
    case paused
    case syncing
    case synced
    case offline      // пиры есть, но никто не на связи
    case noPeers
}

// MARK: - Store

enum StatusDecoder {
    static let shared: JSONDecoder = {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        let precise = ISO8601DateFormatter()
        precise.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let plain = ISO8601DateFormatter()
        d.dateDecodingStrategy = .custom { dec in
            let s = try dec.singleValueContainer().decode(String.self)
            if let date = precise.date(from: s) ?? plain.date(from: s) { return date }
            throw DecodingError.dataCorrupted(.init(codingPath: dec.codingPath, debugDescription: "date \(s)"))
        }
        return d
    }()
}

/// UI-independent rules shared by the menu and the standalone status tests.
enum StatusLogic {
    static func decode(_ data: Data) throws -> DaemonStatus {
        try StatusDecoder.shared.decode(DaemonStatus.self, from: data)
    }

    static func phase(for status: DaemonStatus, hasConfig: Bool, daemonStarting: Bool) -> Phase {
        if status.folderError != nil { return .notRunning }
        guard hasConfig else { return .setup }
        guard status.running else { return daemonStarting ? .starting : .notRunning }
        if status.paused { return .paused }
        if status.peers.isEmpty { return .noPeers }
        if status.syncing { return .syncing }
        if status.peers.contains(where: \.connected) { return .synced }
        return .offline
    }

    static func headlineKey(for status: DaemonStatus) -> String? {
        status.folderError == nil ? nil : "Folder sync stopped"
    }

    static func confirmation(action: FolderAction, status: DaemonStatus) -> FolderConfirmation? {
        guard let error = status.folderError else { return nil }
        return FolderConfirmation(action: action, token: error.token, code: error.code)
    }

    static func confirmationErrorKey(statusCode: Int) -> String? {
        statusCode == 409 ? "Folder state changed. Review the latest status and confirm again." : nil
    }
}


@MainActor
final class Store: ObservableObject {
    @Published private(set) var status = DaemonStatus(running: false)
    @Published private(set) var phase: Phase = .starting
    @Published var lastError: String?
    @Published var pairing: String? // кандидат, для которого открыта сверка ID
    @Published var folderConfirmation: FolderConfirmation?
    @Published private(set) var launchAtLogin = SMAppService.mainApp.status == .enabled
    @Published var tab: MenuTab = MenuTab(rawValue: UserDefaults.standard.string(forKey: "tab") ?? "") ?? .devices {
        didSet { UserDefaults.standard.set(tab.rawValue, forKey: "tab") }
    }
    /// Скорость приёма по пирам, байт/с (сглаженная).
    @Published private(set) var speeds: [String: Double] = [:]
    /// Последний известный прогресс пира: на паузе демон прогресс не отдаёт,
    /// а полоска должна остаться там, где передача остановилась.
    @Published private(set) var heldProgress: [String: DaemonStatus.Progress] = [:]

    private var samples: [String: (bytes: Int64, at: Date)] = [:]

    private let client = ControlClient(socketPath: SharedirPaths.socket)
    private var child: Process?
    private var lastSpawn = Date.distantPast
    private var pollTask: Task<Void, Never>?


    init() {
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                await self?.poll()
                try? await Task.sleep(for: .seconds(1.5))
            }
        }
        NotificationCenter.default.addObserver(forName: NSApplication.willTerminateNotification, object: nil, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated { self?.child?.terminate() }
        }
    }

    var configExists: Bool { FileManager.default.fileExists(atPath: SharedirPaths.config.path) }

    // MARK: Опрос

    func poll() async {
        let client = self.client
        let result: Result<DaemonStatus, Error> = await Task.detached {
            do {
                let data = try client.get("/status")
                return .success(try StatusLogic.decode(data))
            } catch {
                return .failure(error)
            }
        }.value

        switch result {
        case .success(let st):
            track(st)
            if st != status { status = st }
        case .failure:
            status = DaemonStatus(running: false)
            if configExists { startDaemonIfNeeded() }
        }
        let p = computePhase()
        if p != phase { phase = p }
    }

    /// Для отладочного рендера: статус из файла вместо демона.
    func inject(_ st: DaemonStatus) {
        pollTask?.cancel()
        track(st)
        status = st
        phase = computePhase()
    }

    /// Скорость и удержанный прогресс по свежему статусу.
    private func track(_ st: DaemonStatus) {
        let now = Date()
        var speeds = self.speeds, held = heldProgress
        for peer in st.peers {
            if let p = peer.progress {
                held[peer.id] = p
                if let prev = samples[peer.id], p.doneBytes >= prev.bytes {
                    let dt = now.timeIntervalSince(prev.at)
                    if dt > 0.2 {
                        let v = Double(p.doneBytes - prev.bytes) / dt
                        // Экспоненциальное сглаживание, чтобы цифра не прыгала.
                        speeds[peer.id] = speeds[peer.id].map { $0 * 0.6 + v * 0.4 } ?? v
                    }
                }
                samples[peer.id] = (p.doneBytes, now)
            } else {
                samples[peer.id] = nil
                speeds[peer.id] = nil
                if !peer.isPaused { held[peer.id] = nil }
            }
        }
        let ids = Set(st.peers.map(\.id))
        held = held.filter { ids.contains($0.key) }
        if speeds != self.speeds { self.speeds = speeds }
        if held != heldProgress { heldProgress = held }
    }

    /// Прогресс пира для полоски: текущий или удержанный на паузе.
    func progress(for peer: DaemonStatus.Peer) -> DaemonStatus.Progress? {
        peer.progress ?? (peer.isPaused ? heldProgress[peer.id] : nil)
    }

    /// Сумма прогресса по всем пирам — для общей полоски и иконки.
    var totalProgress: DaemonStatus.Progress? {
        let all = status.peers.compactMap { progress(for: $0) }
        guard !all.isEmpty else { return nil }
        return all.reduce(DaemonStatus.Progress(files: 0, doneFiles: 0, bytes: 0, doneBytes: 0)) {
            .init(files: $0.files + $1.files, doneFiles: $0.doneFiles + $1.doneFiles,
                  bytes: $0.bytes + $1.bytes, doneBytes: $0.doneBytes + $1.doneBytes)
        }
    }

    private func computePhase() -> Phase {
        StatusLogic.phase(for: status, hasConfig: configExists, daemonStarting: child?.isRunning == true)
    }

    // MARK: Демон

    /// Путь к sharedir: сначала встроенный в бандл, затем ~/.local/bin.
    private var daemonBinary: URL? {
        if let url = Bundle.main.url(forAuxiliaryExecutable: "sharedir") { return url }
        let local = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".local/bin/sharedir")
        return FileManager.default.isExecutableFile(atPath: local.path) ? local : nil
    }

    /// Поднимает демон, если он не отвечает и не запущен через launchd.
    func startDaemonIfNeeded(force: Bool = false) {
        if child?.isRunning == true { return }
        guard force || Date().timeIntervalSince(lastSpawn) > 5, let bin = daemonBinary else { return }
        lastSpawn = Date()
        let p = Process()
        p.executableURL = bin
        p.arguments = ["run"]
        if FileManager.default.createFile(atPath: SharedirPaths.log.path, contents: nil) || FileManager.default.fileExists(atPath: SharedirPaths.log.path),
           let log = try? FileHandle(forWritingTo: SharedirPaths.log) {
            log.seekToEndOfFile()
            p.standardOutput = log
            p.standardError = log
        }
        do {
            try p.run()
            child = p
        } catch {
            lastError = L("Couldn’t start the daemon: %@", error.localizedDescription)
        }
    }

    private func runCLI(_ args: [String]) throws {
        guard let bin = daemonBinary else { throw ControlClient.Failure.notRunning }
        let p = Process()
        p.executableURL = bin
        p.arguments = args
        let err = Pipe()
        p.standardError = err
        p.standardOutput = Pipe()
        try p.run()
        p.waitUntilExit()
        if p.terminationStatus != 0 {
            let msg = String(data: err.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
            throw ControlClient.Failure.http(Int(p.terminationStatus), msg.replacingOccurrences(of: "sharedir: ", with: "").trimmingCharacters(in: .whitespacesAndNewlines))
        }
    }

    // MARK: Действия

    private func perform(_ action: @escaping @Sendable (ControlClient) throws -> Void, staleTokenAware: Bool = false) {
        let client = self.client
        Task {
            let error: String? = await Task.detached {
                do { try action(client); return nil } catch ControlClient.Failure.http(let code, let msg) {
                    if staleTokenAware, let key = StatusLogic.confirmationErrorKey(statusCode: code) { return L(key) }
                    return L(msg) // ошибки демона приходят по-английски — это ключи перевода
                } catch {
                    return L("The daemon is not responding")
                }
            }.value
            lastError = error
            await poll()
        }
    }

    func setPaused(_ paused: Bool) {
        perform { try $0.post(paused ? "/pause" : "/resume") }
    }

    func requestFolderConfirmation(_ action: FolderAction) {
        folderConfirmation = StatusLogic.confirmation(action: action, status: status)
    }

    func confirmFolder() {
        guard let confirmation = folderConfirmation else { return }
        folderConfirmation = nil
        let action = confirmation.action.rawValue
        let token = confirmation.token
        perform({ try $0.post("/folder/confirm", json: ["token": token, "action": action]) }, staleTokenAware: true)
    }

    func pair(_ id: String) {
        pairing = nil
        perform { try $0.post("/pair", json: ["id": id]) }
    }

    func openFolder() {
        guard !status.folder.isEmpty else { return }
        NSWorkspace.shared.open(URL(fileURLWithPath: status.folder))
    }

    /// Показать файл из ленты в Finder.
    func reveal(_ name: String) {
        guard !status.folder.isEmpty else { return }
        let url = URL(fileURLWithPath: status.folder).appendingPathComponent(name)
        if FileManager.default.fileExists(atPath: url.path) {
            NSWorkspace.shared.activateFileViewerSelecting([url])
        } else {
            NSWorkspace.shared.open(url.deletingLastPathComponent())
        }
    }

    func openLog() {
        NSWorkspace.shared.open(SharedirPaths.log)
    }

    func copyID() {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(Format.prettyID(status.id), forType: .string)
    }

    /// Первый запуск: выбрать папку, создать конфиг и ключ, поднять демон.
    func chooseFolder() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.canCreateDirectories = true
        panel.prompt = L("Sync This Folder")
        panel.message = L("Choose the folder sharedir will keep in sync")
        NSApp.activate()
        guard panel.runModal() == .OK, let url = panel.url else { return }
        do {
            try runCLI(["init", "--folder", url.path])
            lastError = nil
            startDaemonIfNeeded(force: true)
        } catch ControlClient.Failure.http(_, let msg) {
            lastError = L(msg)
        } catch {
            lastError = L("The sharedir binary was not found")
        }
        Task { await poll() }
    }

    func setLaunchAtLogin(_ on: Bool) {
        do {
            if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
            lastError = nil
        } catch {
            lastError = L("Open at Login: %@", error.localizedDescription)
        }
        launchAtLogin = SMAppService.mainApp.status == .enabled
    }
}

enum FolderAction: String {
    case reset, rebind
}

struct FolderConfirmation: Equatable {
    let action: FolderAction
    let token: String
    let code: String
}

// MARK: - Локализация и форматирование

/// Перевод по английскому ключу из Localizable.strings (en — базовый язык).
func L(_ key: String, _ args: CVarArg...) -> String {
    let format = Bundle.main.localizedString(forKey: key, value: key, table: nil)
    return args.isEmpty ? format : String(format: format, locale: Format.locale, arguments: args)
}

enum Format {
    /// Язык, который macOS выбрала для приложения из en/ru, — по нему же
    /// форматируем числа и время, чтобы не смешивать «7:00 PM» с русским текстом.
    static let language = Bundle.main.preferredLocalizations.first ?? "en"
    static let locale = Locale(identifier: language == "ru" ? "ru_RU" : "en_US")

    static func prettyID(_ id: String) -> String {
        stride(from: 0, to: id.count, by: 4).map { i -> String in
            let start = id.index(id.startIndex, offsetBy: i)
            let end = id.index(start, offsetBy: min(4, id.count - i))
            return String(id[start..<end])
        }.joined(separator: "-")
    }

    static func bytes(_ n: Int64) -> String {
        let units = language == "ru" ? ["Б", "КБ", "МБ", "ГБ", "ТБ"] : ["B", "KB", "MB", "GB", "TB"]
        var v = Double(max(n, 0)), i = 0
        while v >= 1000 && i < units.count - 1 { v /= 1000; i += 1 }
        let digits = i == 0 || v >= 100 ? 0 : 1
        return v.formatted(.number.precision(.fractionLength(digits)).locale(locale)) + " " + units[i]
    }

    static func time(_ d: Date) -> String {
        d.formatted(Date.FormatStyle(date: .omitted, time: .shortened).locale(locale))
    }

    static func files(_ n: Int) -> String {
        let num = n.formatted(.number.locale(locale))
        guard language == "ru" else { return n == 1 ? "1 file" : "\(num) files" }
        let mod10 = n % 10, mod100 = n % 100
        if mod10 == 1 && mod100 != 11 { return "\(num) файл" }
        if (2...4).contains(mod10) && !(12...14).contains(mod100) { return "\(num) файла" }
        return "\(num) файлов"
    }

    static func percent(_ f: Double) -> String {
        f.formatted(.percent.precision(.fractionLength(0)).locale(locale))
    }

    static func speed(_ bytesPerSecond: Double) -> String {
        L("%@/s", bytes(Int64(bytesPerSecond)))
    }

    /// «Пн» / «Mon» для даты дня из статуса.
    static func weekday(_ day: String) -> String {
        let p = DateFormatter()
        p.locale = Locale(identifier: "en_US_POSIX")
        p.dateFormat = "yyyy-MM-dd"
        guard let d = p.date(from: day) else { return day }
        let f = DateFormatter()
        f.locale = locale
        f.setLocalizedDateFormatFromTemplate("EEE")
        return f.string(from: d).replacingOccurrences(of: ".", with: "").capitalized(with: locale)
    }

    static func kind(_ code: String) -> String {
        switch code {
        case "video": return L("Video")
        case "images": return L("Images")
        case "docs": return L("Documents")
        case "audio": return L("Audio")
        case "archive": return L("Archives")
        case "code": return L("Code")
        default: return L("Other")
        }
    }

    /// Время события: сегодня — только часы, иначе ещё и дата.
    static func eventTime(_ d: Date) -> String {
        Calendar.current.isDateInToday(d)
            ? time(d)
            : d.formatted(Date.FormatStyle(date: .abbreviated, time: .shortened).locale(locale))
    }

    static func ago(_ date: Date) -> String {
        if Date().timeIntervalSince(date) < 60 { return L("just now") }
        let f = RelativeDateTimeFormatter()
        f.locale = locale
        f.unitsStyle = .short
        return f.localizedString(for: date, relativeTo: Date())
    }

    static func abbreviatedPath(_ p: String) -> String {
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        return p.hasPrefix(home) ? "~" + p.dropFirst(home.count) : p
    }
}
