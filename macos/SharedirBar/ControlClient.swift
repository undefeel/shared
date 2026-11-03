import CryptoKit
import Foundation

/// HTTP поверх unix-сокета демона. URLSession unix-сокеты не умеет, поэтому
/// говорим HTTP/1.0 руками: сервер Go отвечает без chunked и закрывает
/// соединение, так что ответ — это всё до EOF.
struct ControlClient {
    enum Failure: Error {
        case notRunning
        case http(Int, String)
    }

    let socketPath: String

    func get(_ path: String) throws -> Data {
        try request("GET", path, body: nil)
    }

    func post(_ path: String, json: [String: String]? = nil) throws {
        let body = try json.map { try JSONSerialization.data(withJSONObject: $0) }
        _ = try request("POST", path, body: body)
    }

    private func request(_ method: String, _ path: String, body: Data?) throws -> Data {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw Failure.notRunning }
        defer { close(fd) }

        var timeout = timeval(tv_sec: 5, tv_usec: 0)
        setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))
        setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))

        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let pathBytes = Array(socketPath.utf8)
        guard pathBytes.count < MemoryLayout.size(ofValue: addr.sun_path) else { throw Failure.notRunning }
        withUnsafeMutableBytes(of: &addr.sun_path) { raw in
            raw.copyBytes(from: pathBytes)
            raw[pathBytes.count] = 0
        }
        let connected = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard connected == 0 else { throw Failure.notRunning }

        var head = "\(method) \(path) HTTP/1.0\r\nHost: sharedir\r\n"
        if let body {
            head += "Content-Type: application/json\r\nContent-Length: \(body.count)\r\n"
        }
        head += "\r\n"
        var out = Data(head.utf8)
        if let body { out.append(body) }
        let sent = out.withUnsafeBytes { send(fd, $0.baseAddress, $0.count, 0) }
        guard sent == out.count else { throw Failure.notRunning }

        var response = Data()
        var buf = [UInt8](repeating: 0, count: 64 * 1024)
        while true {
            let n = recv(fd, &buf, buf.count, 0)
            if n <= 0 { break }
            response.append(buf, count: n)
        }

        guard let sep = response.range(of: Data("\r\n\r\n".utf8)),
              let statusLine = String(data: response[..<sep.lowerBound], encoding: .utf8)?
                .split(separator: "\r\n").first,
              let code = Int(statusLine.split(separator: " ").dropFirst().first ?? "")
        else { throw Failure.notRunning }
        let payload = response[sep.upperBound...]
        guard code == 200 else {
            let msg = String(data: payload, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
            throw Failure.http(code, msg)
        }
        return Data(payload)
    }
}

/// Пути sharedir — те же правила, что в internal/config.
enum SharedirPaths {
    static var home: URL {
        if let h = ProcessInfo.processInfo.environment["SHAREDIR_HOME"], !h.isEmpty {
            return URL(fileURLWithPath: h)
        }
        return FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("sharedir")
    }

    static var config: URL { home.appendingPathComponent("config.json") }

    static var log: URL {
        FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Library/Logs/sharedir.log")
    }

    /// Unix-сокеты ограничены ~104 байтами: длинный путь демон уносит в TMPDIR.
    static var socket: String {
        let p = home.appendingPathComponent("ctl.sock").path
        if p.utf8.count < 100 { return p }
        let digest = SHA256.hash(data: Data(home.path.utf8))
        let hex = digest.prefix(6).map { String(format: "%02x", $0) }.joined()
        return URL(fileURLWithPath: NSTemporaryDirectory()).appendingPathComponent("sharedir-\(hex).sock").path
    }
}
