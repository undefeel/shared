import Foundation

@main
enum StatusTests {
    static func main() throws {
        try testLegacyStatusDecodesWithoutOptionalErrors()
        try testBlockedFolderErrorDecodes()
        try testScanErrorDecodes()
        try testResetConfirmationUsesStatusToken()
        try testRebindConfirmationUsesStatusToken()
        try testStaleTokenMaps409ToReviewMessage()
        try testOtherFailureIsNotTreatedAsStaleToken()
        try testBlockedFolderNeverAppearsSynced()
        try testIncompatible()
        print("9 macOS status tests passed")
    }


    static func testIncompatible() throws {
        var status = try decode(#"{"running":true,"syncing":true,"peers":[{"id":"v1","name":"Linux","connected":true,"syncing":false,"state":"incompatible","error_code":"protocol_incompatible","error_message":"expected 2","expected_protocol":2,"received_protocol":"1"}]}"#)
        try expect(status.peers[0].expectedProtocol == 2, "expected protocol")
        try expect(status.peers[0].receivedProtocol == "1", "v1 protocol")
        try expect(status.peers[0].errorMessage == "expected 2", "message decoded")
        try expect(StatusLogic.phase(for: status, hasConfig: true, daemonStarting: false) == .incompatible, "incompatible overrides syncing and connected")
        status.paused = true
        try expect(StatusLogic.phase(for: status, hasConfig: true, daemonStarting: false) == .paused, "pause wins")
        status.folderError = .init(code: "root_changed", message: "root", token: "token")
        try expect(StatusLogic.phase(for: status, hasConfig: true, daemonStarting: false) == .notRunning, "root wins")
        for language in ["en", "ru"] {
            let text = try String(contentsOfFile: "macos/Resources/\(language).lproj/Localizable.strings", encoding: .utf8)
            try expect(text.contains("\"Update the other machine to the same sharedir version\" = "), "update translation")
        }
    }

    static func testLegacyStatusDecodesWithoutOptionalErrors() throws {
        let status = try decode(#"{"running":true,"peers":[]}"#)
        try expect(status.running, "legacy status running")
        try expect(status.folderError == nil, "legacy status has no folder_error")
        try expect(status.scanError == nil, "legacy status has no scan_error")
    }

    static func testBlockedFolderErrorDecodes() throws {
        let status = try decode(#"{"running":true,"folder_error":{"code":"root_empty","message":"empty","token":"t-1"}}"#)
        try expect(status.folderError == .init(code: "root_empty", message: "empty", token: "t-1"), "blocked status error fields")
    }

    static func testScanErrorDecodes() throws {
        let status = try decode(#"{"running":true,"scan_error":{"message":"read failed","path":"dir/file","time":"2026-11-04T12:00:00Z"}}"#)
        try expect(status.scanError?.message == "read failed", "scan error message")
        try expect(status.scanError?.path == "dir/file", "scan error path")
        try expect(status.scanError?.time != nil, "scan error time")
    }

    static func testResetConfirmationUsesStatusToken() throws {
        let status = try blockedStatus()
        let confirmation = StatusLogic.confirmation(action: .reset, status: status)
        try expect(confirmation == .init(action: .reset, token: "token-1", code: "root_empty"), "reset confirmation fields")
    }

    static func testRebindConfirmationUsesStatusToken() throws {
        let status = try blockedStatus()
        let confirmation = StatusLogic.confirmation(action: .rebind, status: status)
        try expect(confirmation == .init(action: .rebind, token: "token-1", code: "root_empty"), "rebind confirmation fields")
    }

    static func testStaleTokenMaps409ToReviewMessage() throws {
        try expect(StatusLogic.confirmationErrorKey(statusCode: 409) == "Folder state changed. Review the latest status and confirm again.", "stale token message")
    }

    static func testOtherFailureIsNotTreatedAsStaleToken() throws {
        try expect(StatusLogic.confirmationErrorKey(statusCode: 400) == nil, "400 is not stale token")
    }

    static func testBlockedFolderNeverAppearsSynced() throws {
        let status = try decode(#"{"running":true,"folder_error":{"code":"root_changed","message":"changed","token":"t-1"},"peers":[{"id":"peer","name":"Peer","connected":true,"syncing":false,"state":"synced"}]}"#)
        let phase = StatusLogic.phase(for: status, hasConfig: true, daemonStarting: false)
        try expect(phase != .synced, "blocked folder phase")
        try expect(StatusLogic.headlineKey(for: status) == "Folder sync stopped", "blocked folder headline")
    }

    private static func blockedStatus() throws -> DaemonStatus {
        try decode(#"{"running":true,"folder_error":{"code":"root_empty","message":"empty","token":"token-1"}}"#)
    }

    private static func decode(_ json: String) throws -> DaemonStatus {
        try StatusLogic.decode(Data(json.utf8))
    }

    private static func expect(_ condition: @autoclosure () -> Bool, _ message: String) throws {
        guard condition() else { throw TestFailure(message) }
    }

    private struct TestFailure: Error, CustomStringConvertible {
        let message: String
        init(_ message: String) { self.message = message }
        var description: String { message }
    }
}
