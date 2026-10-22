import Foundation

/// Something waiting for the user on one of the devices.
struct Pending: Equatable {
    let id: String
    let node: String      // device id; "local" for this Mac
    let nodeName: String
    let sessionID: String
    let title: String
    let kind: String      // permission | question | plan
    let tool: String

    var heading: String {
        switch kind {
        case "question": return "Agent 在提问"
        case "plan": return "计划待批准"
        default: return "需要审批"
        }
    }
}

/// The Go console bundled in the app (Contents/Resources/agentconsole) and a
/// small client for its API. The console itself runs as a launchd service, so
/// agents keep working when the app quits.
final class Console {
    let binary: URL
    private(set) var port = 8899
    private(set) var dataDir = ""
    // Its own cookie jar: the app's polling session is separate from the
    // window's (WKWebView keeps its own).
    private let http = URLSession(configuration: .ephemeral)

    init?() {
        guard let b = Bundle.main.url(forResource: "agentconsole", withExtension: nil) else { return nil }
        binary = b
    }

    /// Starts the background service, or moves it onto this app's build.
    /// A warning from the last start, e.g. the service being switched off in
    /// Login Items; shown to the user once.
    private(set) var warning = ""

    /// Stops the console when the app quits. Agent tasks are separate
    /// processes and keep running; their output is picked up next launch.
    func stop() {
        let p = Process()
        p.executableURL = binary
        p.arguments = ["client", "stop"]
        p.standardOutput = FileHandle.nullDevice
        p.standardError = FileHandle.nullDevice
        try? p.run()
        p.waitUntilExit()
    }

    func ensureRunning() throws {
        let p = Process()
        p.executableURL = binary
        p.arguments = ["client", "--no-open"]
        let out = Pipe()
        p.standardOutput = out
        p.standardError = out
        try p.run()
        p.waitUntilExit()
        let text = String(data: out.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
        // Kept for diagnosis: warnings (e.g. a service-manager fallback) are
        // printed even when the start succeeds.
        let log = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".agentconsole/log/app.log")
        if let h = try? FileHandle(forWritingTo: log) ?? nil {
            h.seekToEndOfFile()
            h.write(Data("\(Date()) client: \(text)\n".utf8))
            try? h.close()
        } else {
            try? Data("\(Date()) client: \(text)\n".utf8).write(to: log)
        }
        warning = text.split(separator: "\n").first(where: { $0.hasPrefix("注意：") }).map {
            String($0.dropFirst(3))
        } ?? ""
        if p.terminationStatus != 0 {
            throw NSError(domain: "AgentConsole", code: Int(p.terminationStatus),
                          userInfo: [NSLocalizedDescriptionKey: text.isEmpty ? "后台服务启动失败" : text])
        }
        loadConfig()
    }

    private func loadConfig() {
        let url = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".agentconsole/config.json")
        guard let data = try? Data(contentsOf: url),
              let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { return }
        if let p = obj["port"] as? Int { port = p }
        if let d = obj["data_dir"] as? String { dataDir = d }
    }

    var baseURL: URL { URL(string: "http://127.0.0.1:\(port)")! }

    /// A one-time login link; each link works once.
    func loginURL() -> URL? {
        let path = (dataDir as NSString).appendingPathComponent("login-ticket")
        guard let t = try? String(contentsOfFile: path, encoding: .utf8)
            .trimmingCharacters(in: .whitespacesAndNewlines), !t.isEmpty else { return nil }
        return URL(string: "http://127.0.0.1:\(port)/api/login/ticket?t=\(t)")
    }

    func login(_ done: @escaping (Bool) -> Void) {
        guard let url = loginURL() else { done(false); return }
        http.dataTask(with: url) { _, resp, _ in
            done((resp as? HTTPURLResponse)?.statusCode == 200)
        }.resume()
    }

    private func get(_ path: String, _ done: @escaping (Any?, Int) -> Void) {
        var req = URLRequest(url: baseURL.appendingPathComponent(path))
        req.timeoutInterval = 15
        http.dataTask(with: req) { data, resp, _ in
            let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
            let obj = data.flatMap { try? JSONSerialization.jsonObject(with: $0) }
            done(obj, code)
        }.resume()
    }

    /// Pending approvals and questions across every device. Logs in again
    /// when the session cookie is gone (e.g. after a password change).
    func pending(_ done: @escaping ([Pending]) -> Void) {
        get("api/nodes") { obj, code in
            if code == 401 {
                self.login { ok in if ok { self.pending(done) } else { done([]) } }
                return
            }
            let nodes = ((obj as? [String: Any])?["nodes"] as? [[String: Any]] ?? [])
                .filter { $0["online"] as? Bool == true }
            let group = DispatchGroup()
            let lock = NSLock()
            var all: [Pending] = []
            for n in nodes {
                let id = n["id"] as? String ?? "local"
                let name = n["name"] as? String ?? id
                let local = n["local"] as? Bool ?? false
                group.enter()
                self.get(local ? "api/permissions" : "n/\(id)/api/permissions") { obj, _ in
                    let perms = (obj as? [String: Any])?["permissions"] as? [[String: Any]] ?? []
                    let items = perms.filter { $0["status"] as? String == "pending" }.map { p in
                        Pending(id: p["id"] as? String ?? "", node: local ? "local" : id, nodeName: name,
                                sessionID: p["session_id"] as? String ?? "",
                                title: p["session_title"] as? String ?? "会话",
                                kind: p["kind"] as? String ?? "permission",
                                tool: p["tool_name"] as? String ?? "")
                    }
                    lock.lock(); all += items; lock.unlock()
                    group.leave()
                }
            }
            group.notify(queue: .main) { done(all.sorted { $0.id < $1.id }) }
        }
    }
}
