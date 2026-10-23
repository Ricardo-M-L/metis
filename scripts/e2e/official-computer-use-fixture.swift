// Owned, opaque, primary-display fixture. No user files, clipboard or TCC.
import AppKit

final class FixtureWindow: NSWindow {
    override var canBecomeKey: Bool { true }
    override var canBecomeMain: Bool { true }
}

final class PatternView: NSView {
    let bits: Int
    init(frame: NSRect, bits: Int) { self.bits = bits; super.init(frame: frame) }
    required init?(coder: NSCoder) { fatalError() }
    override func draw(_ dirtyRect: NSRect) {
        NSColor(srgbRed: 24/255, green: 32/255, blue: 48/255, alpha: 1).setFill()
        bounds.fill()
        for i in 0..<8 {
            let one = (bits & (1 << i)) != 0
            NSColor(srgbRed: one ? 25/255 : 220/255,
                    green: one ? 200/255 : 35/255, blue: one ? 85/255 : 50/255, alpha: 1).setFill()
            NSRect(x: bounds.width * (0.1 + Double(i) * 0.1), y: bounds.height * 0.45,
                   width: bounds.width * 0.075, height: bounds.height * 0.1).fill()
        }
    }
}

final class OfficialFixture: NSObject, NSApplicationDelegate {
    let statePath: String
    let nonce: String
    var window: NSWindow!
    var plain: NSTextField!
    var button: NSButton!
    var counter: NSTextField!
    var count = 0
    var ticks = 0
    var timer: Timer?
    var focusSerial = 0
    var focusObserver: NSObjectProtocol?
    var inputEvents = 0
    init(_ state: String, _ nonce: String) { self.statePath = state; self.nonce = nonce }

    func applicationDidFinishLaunching(_ note: Notification) {
        let menu = NSMenu()
        let appItem = NSMenuItem()
        let appMenu = NSMenu()
        appMenu.addItem(withTitle: "Quit", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = appMenu; menu.addItem(appItem)
        let edit = NSMenuItem(title: "Edit", action: nil, keyEquivalent: "")
        let editMenu = NSMenu(title: "Edit")
        let all = NSMenuItem(title: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        all.keyEquivalentModifierMask = .command; all.target = nil
        editMenu.addItem(all); edit.submenu = editMenu; menu.addItem(edit); NSApp.mainMenu = menu
        guard let screen = NSScreen.screens.first else { exit(3) }
        let frame = screen.frame
        window = FixtureWindow(contentRect: frame, styleMask: [.borderless], backing: .buffered, defer: false)
        window.title = ProcessInfo.processInfo.processName
        window.isOpaque = true; window.backgroundColor = .black
        window.level = NSWindow.Level(rawValue: NSWindow.Level.mainMenu.rawValue + 1)
        window.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary]
        window.isReleasedWhenClosed = false
        let content = PatternView(frame: NSRect(origin: .zero, size: frame.size), bits: Int(nonce.prefix(2), radix: 16)!)
        window.contentView = content
        let heading = NSTextField(labelWithString: "METIS official Computer Use acceptance — owned disposable window " + nonce)
        heading.textColor = .white; heading.font = .systemFont(ofSize: 22)
        heading.frame = NSRect(x: 80, y: frame.height - 100, width: frame.width - 160, height: 40)
        content.addSubview(heading)
        plain = NSTextField(frame: NSRect(x: 100, y: frame.height - 210, width: min(700, frame.width - 200), height: 46))
        plain.font = .systemFont(ofSize: 24); plain.stringValue = "fixture initial value"
        plain.setAccessibilityLabel("Fixture plain input"); content.addSubview(plain)
        button = NSButton(title: "Increment fixture counter", target: self, action: #selector(increment))
        button.frame = NSRect(x: 100, y: frame.height - 310, width: 330, height: 48)
        button.bezelStyle = .rounded; content.addSubview(button)
        counter = NSTextField(labelWithString: "Fixture count 0")
        counter.frame = NSRect(x: 470, y: frame.height - 305, width: 350, height: 40)
        counter.textColor = .white; counter.font = .systemFont(ofSize: 24); content.addSubview(counter)
        focusObserver = NSWorkspace.shared.notificationCenter.addObserver(forName: NSWorkspace.didActivateApplicationNotification,
            object: nil, queue: .main) { [weak self] _ in self?.focusSerial += 1; self?.publish() }
        window.makeKeyAndOrderFront(nil); window.makeFirstResponder(plain)
        NSApp.activate(ignoringOtherApps: true)
        publish(); timer = Timer.scheduledTimer(withTimeInterval: 0.04, repeats: true) { [weak self] _ in self?.publish() }
    }

    @objc func increment() { count += 1; counter.stringValue = "Fixture count \(count)"; publish() }
    func point(_ view: NSView) -> [String: Int] {
        let local = NSRect(x: view.bounds.midX, y: view.bounds.midY, width: 0, height: 0)
        let rect = window.convertToScreen(view.convert(local, to: nil))
        return ["x": Int(rect.minX.rounded()), "y": Int((NSScreen.screens.first!.frame.maxY - rect.minY).rounded())]
    }
    func publish() {
        guard plain != nil else { return }
        ticks += 1
        let front = NSWorkspace.shared.frontmostApplication
        let frame = NSScreen.screens.first!.frame
        let state: [String: Any] = ["schema_version": 1, "nonce": nonce,
            "pid": Int(ProcessInfo.processInfo.processIdentifier), "name": ProcessInfo.processInfo.processName,
            "bundle_id": Bundle.main.bundleIdentifier ?? "", "input": plain.currentEditor()?.string ?? plain.stringValue,
            "counter": count, "ticks": ticks, "active": NSApp.isActive,
            "foreground_pid": Int(front?.processIdentifier ?? 0), "foreground_bundle_id": front?.bundleIdentifier ?? "",
            "uptime": ProcessInfo.processInfo.systemUptime, "focus_serial": focusSerial,
            "selection_length": plain.currentEditor()?.selectedRange.length ?? -1,
            "screen": ["x": Int(frame.minX), "y": Int(frame.minY), "width": Int(frame.width), "height": Int(frame.height)],
            "input_point": point(plain), "button_point": point(button), "opaque_full_display": true]
        if let data = try? JSONSerialization.data(withJSONObject: state, options: [.sortedKeys]) {
            try? data.write(to: URL(fileURLWithPath: statePath), options: [.atomic])
            try? FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: statePath)
        }
    }
    func applicationWillTerminate(_ note: Notification) { timer?.invalidate() }
}
guard CommandLine.arguments.count == 3, CommandLine.arguments[1].hasPrefix("/") else { exit(2) }
let application = NSApplication.shared
application.setActivationPolicy(.regular)
let delegate = OfficialFixture(CommandLine.arguments[1], CommandLine.arguments[2])
application.delegate = delegate
application.run()
