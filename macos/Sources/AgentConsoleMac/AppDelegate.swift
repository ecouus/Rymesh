import AppKit
import ServiceManagement
import UserNotifications
import WebKit

/// Menu bar item, console window and notifications. Quitting the app leaves
/// the console service (and every running agent) alone.
final class AppDelegate: NSObject, NSApplicationDelegate, WKNavigationDelegate, WKUIDelegate,
    UNUserNotificationCenterDelegate {
    private var console: Console!
    private var window: NSWindow?
    private var web: WKWebView?
    private var pageReady = false
    private var afterLoad: [String] = []      // JS to run once the page is up
    private var status: NSStatusItem!
    private var pending: [Pending] = []
    private var notified = Set<String>()
    private var timer: Timer?

    // MARK: Launch

    func applicationDidFinishLaunching(_ note: Notification) {
        buildMainMenu()
        status = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        status.button?.image = NSImage(systemSymbolName: "terminal", accessibilityDescription: "Agent Console")
        status.button?.imagePosition = .imageLeading
        rebuildStatusMenu()

        let center = UNUserNotificationCenter.current()
        center.delegate = self
        center.requestAuthorization(options: [.alert, .sound, .badge]) { _, _ in }

        guard let c = Console() else {
            fail("应用包里缺少 agentconsole 程序，请重新下载。")
            return
        }
        console = c
        DispatchQueue.global(qos: .userInitiated).async {
            do {
                try c.ensureRunning()
            } catch {
                DispatchQueue.main.async { self.fail(error.localizedDescription) }
                return
            }
            // The poller logs in first; the window then takes the next ticket.
            c.login { _ in
                DispatchQueue.main.async {
                    self.showWindow()
                    self.poll()
                    self.timer = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { _ in self.poll() }
                }
            }
        }
    }

    private func fail(_ message: String) {
        let a = NSAlert()
        a.messageText = "Agent Console 无法启动"
        a.informativeText = message
        a.runModal()
        NSApp.terminate(nil)
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows: Bool) -> Bool {
        showWindow()
        return true
    }

    // MARK: Window

    @objc func showWindow() {
        guard console != nil else { return }
        if window == nil {
            let cfg = WKWebViewConfiguration()
            cfg.websiteDataStore = .default()
            let wv = WKWebView(frame: .zero, configuration: cfg)
            wv.navigationDelegate = self
            wv.uiDelegate = self
            wv.allowsBackForwardNavigationGestures = true
            let w = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1180, height: 820),
                             styleMask: [.titled, .closable, .miniaturizable, .resizable],
                             backing: .buffered, defer: false)
            w.title = "Agent Console"
            w.contentView = wv
            w.isReleasedWhenClosed = false
            w.center()
            w.setFrameAutosaveName("AgentConsoleMain")
            window = w
            web = wv
            load()
        }
        window?.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    private func load() {
        pageReady = false
        if let url = console.loginURL() {
            web?.load(URLRequest(url: url))
        } else {
            web?.load(URLRequest(url: console.baseURL))
        }
    }

    @objc func reload() { load() }

    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        pageReady = true
        let queued = afterLoad
        afterLoad.removeAll()
        // Give the page a moment to boot before acting on it.
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.2) {
            queued.forEach { webView.evaluateJavaScript($0) }
        }
    }

    // Links out of the console open in the default browser.
    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        if let url = action.request.url, url.host != "127.0.0.1", url.scheme?.hasPrefix("http") == true {
            NSWorkspace.shared.open(url)
            decisionHandler(.cancel)
            return
        }
        decisionHandler(.allow)
    }

    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
                 for action: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        if let url = action.request.url { NSWorkspace.shared.open(url) }
        return nil
    }

    private func open(_ p: Pending) {
        showWindow()
        let js = "window.acOpen && window.acOpen(\(jsString(p.node)), \(jsString(p.sessionID)), \(jsString(p.id)))"
        if pageReady { web?.evaluateJavaScript(js) } else { afterLoad.append(js) }
    }

    private func jsString(_ s: String) -> String {
        let data = try? JSONSerialization.data(withJSONObject: [s])
        let arr = data.flatMap { String(data: $0, encoding: .utf8) } ?? "[\"\"]"
        return String(arr.dropFirst().dropLast())
    }

    // MARK: Pending items: menu bar, Dock badge, notifications

    private func poll() {
        console.pending { items in
            let fresh = items.filter { !self.notified.contains($0.id) }
            for p in fresh { self.notify(p) }
            self.notified.formUnion(fresh.map(\.id))
            // Resolved ones leave Notification Center too.
            let gone = self.pending.map(\.id).filter { id in !items.contains { $0.id == id } }
            if !gone.isEmpty {
                UNUserNotificationCenter.current().removeDeliveredNotifications(withIdentifiers: gone)
            }
            if items != self.pending {
                self.pending = items
                self.rebuildStatusMenu()
            }
            let n = items.count
            self.status.button?.title = n > 0 ? " \(n)" : ""
            NSApp.dockTile.badgeLabel = n > 0 ? "\(n)" : nil
        }
    }

    private func notify(_ p: Pending) {
        let c = UNMutableNotificationContent()
        c.title = p.heading
        c.body = p.nodeName == "" ? p.title : "\(p.nodeName) · \(p.title)"
        c.sound = .default
        c.userInfo = ["id": p.id, "node": p.node, "sid": p.sessionID]
        UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: p.id, content: c, trigger: nil))
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
                                withCompletionHandler done: @escaping () -> Void) {
        let info = response.notification.request.content.userInfo
        let p = Pending(id: info["id"] as? String ?? "", node: info["node"] as? String ?? "local", nodeName: "",
                        sessionID: info["sid"] as? String ?? "", title: "", kind: "", tool: "")
        DispatchQueue.main.async { self.open(p) }
        done()
    }

    // Show banners while the app is frontmost too, unless the window is the
    // focused one (the card is right there).
    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                withCompletionHandler done: @escaping (UNNotificationPresentationOptions) -> Void) {
        done(window?.isKeyWindow == true ? [] : [.banner, .sound])
    }

    private func rebuildStatusMenu() {
        let m = NSMenu()
        m.addItem(withTitle: "打开 Agent Console", action: #selector(showWindow), keyEquivalent: "o").target = self
        m.addItem(.separator())
        if pending.isEmpty {
            m.addItem(NSMenuItem(title: "没有待处理的请求", action: nil, keyEquivalent: ""))
        } else {
            for (i, p) in pending.prefix(8).enumerated() {
                let item = NSMenuItem(title: "\(p.heading) · \(p.nodeName) · \(p.title)",
                                      action: #selector(openPending(_:)), keyEquivalent: "")
                item.target = self
                item.tag = i
                m.addItem(item)
            }
        }
        m.addItem(.separator())
        let login = NSMenuItem(title: "登录时打开", action: #selector(toggleLoginItem), keyEquivalent: "")
        login.target = self
        login.state = SMAppService.mainApp.status == .enabled ? .on : .off
        m.addItem(login)
        m.addItem(withTitle: "退出（Agent 任务继续在后台运行）", action: #selector(NSApplication.terminate(_:)),
                  keyEquivalent: "q")
        status.menu = m
    }

    @objc private func openPending(_ sender: NSMenuItem) {
        guard sender.tag < pending.count else { return }
        open(pending[sender.tag])
    }

    @objc private func toggleLoginItem() {
        do {
            if SMAppService.mainApp.status == .enabled {
                try SMAppService.mainApp.unregister()
            } else {
                try SMAppService.mainApp.register()
            }
        } catch {
            let a = NSAlert()
            a.messageText = "无法更改登录项"
            a.informativeText = error.localizedDescription + "\n（请先把应用放到“应用程序”文件夹）"
            a.runModal()
        }
        rebuildStatusMenu()
    }

    // MARK: Main menu (Edit keeps ⌘C/⌘V working inside the web view)

    private func buildMainMenu() {
        let main = NSMenu()
        let appItem = NSMenuItem()
        let appMenu = NSMenu()
        appMenu.addItem(withTitle: "关于 Agent Console", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "隐藏 Agent Console", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        appMenu.addItem(withTitle: "退出 Agent Console", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = appMenu
        main.addItem(appItem)

        let editItem = NSMenuItem()
        let edit = NSMenu(title: "编辑")
        edit.addItem(withTitle: "撤销", action: Selector(("undo:")), keyEquivalent: "z")
        edit.addItem(withTitle: "重做", action: Selector(("redo:")), keyEquivalent: "Z")
        edit.addItem(.separator())
        edit.addItem(withTitle: "剪切", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "拷贝", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "粘贴", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: "全选", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editItem.submenu = edit
        main.addItem(editItem)

        let viewItem = NSMenuItem()
        let view = NSMenu(title: "显示")
        view.addItem(withTitle: "重新载入", action: #selector(reload), keyEquivalent: "r").target = self
        viewItem.submenu = view
        main.addItem(viewItem)

        let winItem = NSMenuItem()
        let win = NSMenu(title: "窗口")
        win.addItem(withTitle: "关闭", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        win.addItem(withTitle: "最小化", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        winItem.submenu = win
        main.addItem(winItem)
        NSApp.mainMenu = main
        NSApp.windowsMenu = win
    }
}
