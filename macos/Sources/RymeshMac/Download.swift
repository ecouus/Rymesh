import AppKit
import WebKit

/// Downloads a file from the console into ~/Downloads for the page's file
/// view, with the page's login cookie, reporting progress to
/// window.acDownload(id, done, total, state, info).
final class Downloader: NSObject, URLSessionDownloadDelegate {
    private let report: (String, Int64, Int64, String?, String?) -> Void
    private var session: URLSession!
    private var tasks: [Int: (id: String, name: String)] = [:]
    private var lastReport: [Int: Date] = [:]
    private(set) var finished: [String: URL] = [:]

    init(report: @escaping (String, Int64, Int64, String?, String?) -> Void) {
        self.report = report
        super.init()
        session = URLSession(configuration: .ephemeral, delegate: self, delegateQueue: .main)
    }

    func start(id: String, path: String, name: String, base: URL, cookies: WKHTTPCookieStore) {
        guard path.hasPrefix("/"), let url = URL(string: path, relativeTo: base)?.absoluteURL else {
            report(id, 0, 0, "error", "地址无效") // translated by the page
            return
        }
        cookies.getAllCookies { all in
            var req = URLRequest(url: url)
            let mine = all.filter { $0.domain == url.host }
            HTTPCookie.requestHeaderFields(with: mine).forEach { req.setValue($1, forHTTPHeaderField: $0) }
            req.setValue("identity", forHTTPHeaderField: "Accept-Encoding")
            let task = self.session.downloadTask(with: req)
            self.tasks[task.taskIdentifier] = (id, name)
            task.resume()
        }
    }

    func reveal(id: String) {
        if let url = finished[id] { NSWorkspace.shared.activateFileViewerSelecting([url]) }
    }

    func urlSession(_ s: URLSession, downloadTask t: URLSessionDownloadTask, didWriteData _: Int64,
                    totalBytesWritten done: Int64, totalBytesExpectedToWrite total: Int64) {
        guard let job = tasks[t.taskIdentifier] else { return }
        let now = Date()
        if let last = lastReport[t.taskIdentifier], now.timeIntervalSince(last) < 0.25 { return }
        lastReport[t.taskIdentifier] = now
        report(job.id, done, total, nil, nil)
    }

    func urlSession(_ s: URLSession, downloadTask t: URLSessionDownloadTask, didFinishDownloadingTo tmp: URL) {
        guard let job = tasks[t.taskIdentifier] else { return }
        let code = (t.response as? HTTPURLResponse)?.statusCode ?? 0
        if code != 200 {
            var msg = "HTTP \(code)"
            if let data = try? Data(contentsOf: tmp),
               let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
               let e = obj["error"] as? String { msg = e }
            report(job.id, 0, 0, "error", msg)
            return
        }
        do {
            let dir = try FileManager.default.url(for: .downloadsDirectory, in: .userDomainMask,
                                                  appropriateFor: nil, create: true)
            let dest = Self.freeURL(in: dir, name: job.name)
            try FileManager.default.moveItem(at: tmp, to: dest)
            finished[job.id] = dest
            report(job.id, t.countOfBytesReceived, t.countOfBytesReceived, "done", "“下载”文件夹")
        } catch {
            report(job.id, 0, 0, "error", error.localizedDescription)
        }
    }

    func urlSession(_ s: URLSession, task t: URLSessionTask, didCompleteWithError error: Error?) {
        defer { tasks[t.taskIdentifier] = nil; lastReport[t.taskIdentifier] = nil }
        if let error = error, let job = tasks[t.taskIdentifier] {
            report(job.id, 0, 0, "error", error.localizedDescription)
        }
    }

    /// name, or "name (1).ext", "name (2).ext"… whichever is free, as Safari does.
    static func freeURL(in dir: URL, name: String) -> URL {
        let safe = name.replacingOccurrences(of: "/", with: "_")
        let ext = (safe as NSString).pathExtension
        let stem = (safe as NSString).deletingPathExtension
        var url = dir.appendingPathComponent(safe)
        var i = 1
        while FileManager.default.fileExists(atPath: url.path) {
            url = dir.appendingPathComponent(ext.isEmpty ? "\(stem) (\(i))" : "\(stem) (\(i)).\(ext)")
            i += 1
        }
        return url
    }
}
