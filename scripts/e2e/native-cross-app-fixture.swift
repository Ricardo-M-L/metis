// Disposable AppKit fixture for computer_use_cross_app_acceptance.py.
// Built twice inside uniquely named .app bundles. It never opens user files,
// reads the clipboard, requests TCC access, or changes another application.
import AppKit

final class CrossAppFixture: NSObject, NSApplicationDelegate {
    var window: NSWindow!
    var plain: NSTextField!
    var counter: NSTextField!
    var count = 0
    var ticks = 0
    var timer: Timer?
    var activationObserver: NSObjectProtocol?
    var focusEvents: [[String: Any]] = []
    var focusEventsDropped = 0
    var commandAMonitor: Any?
    var commandAEvents: [[String: Any]] = []
    var commandAEventSequence = 0
    var commandAEventsDropped = 0
    let statePath: String
    let nonce: String

    init(statePath: String, nonce: String) {
        self.statePath = statePath
        self.nonce = nonce
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        // A normal AppKit menu gives Command+A its standard responder-chain
        // action. No selection is made by setup or the diagnostic monitor.
        let menu = NSMenu()
        let appItem = NSMenuItem()
        appItem.submenu = NSMenu()
        menu.addItem(appItem)
        let editItem = NSMenuItem(title: "Edit", action: nil, keyEquivalent: "")
        let editMenu = NSMenu(title: "Edit")
        let selectAll = NSMenuItem(title: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        selectAll.keyEquivalentModifierMask = .command
        selectAll.target = nil
        editMenu.addItem(selectAll)
        editItem.submenu = editMenu
        menu.addItem(editItem)
        NSApp.mainMenu = menu
        commandAMonitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { [weak self] event in
            let modifiers = event.modifierFlags.intersection([.command, .control, .option, .shift])
            if event.keyCode == 0 && modifiers == .command {
                self?.recordCommandA(keycode: event.keyCode, flags: event.modifierFlags.rawValue)
            }
            return event
        }
        // Read-only test diagnostics. Keep only bounded PID/bundle metadata;
        // never inspect another application's name, windows or content, and
        // never reactivate a fixture when it loses focus.
        activationObserver = NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.didActivateApplicationNotification, object: nil, queue: .main
        ) { [weak self] notification in
            let activated = notification.userInfo?[NSWorkspace.applicationUserInfoKey] as? NSRunningApplication
            self?.recordFocus("workspace_activation", activated: activated)
        }
        recordFocus("fixture_started")
        let frame = NSScreen.main?.visibleFrame ?? NSRect(x: 0, y: 0, width: 1000, height: 700)
        let origin = NSPoint(x: frame.midX - 280, y: frame.midY - 160)
        window = NSWindow(contentRect: NSRect(origin: origin, size: NSSize(width: 560, height: 320)),
                          styleMask: [.titled, .closable], backing: .buffered, defer: false)
        window.title = ProcessInfo.processInfo.processName
        window.isReleasedWhenClosed = false
        let content = NSView(frame: NSRect(x: 0, y: 0, width: 560, height: 320))
        window.contentView = content
        let heading = NSTextField(labelWithString: "Disposable Metis cross-app acceptance")
        heading.frame = NSRect(x: 24, y: 266, width: 510, height: 26)
        content.addSubview(heading)
        plain = NSTextField(frame: NSRect(x: 24, y: 210, width: 510, height: 32))
        plain.stringValue = "fixture initial value"
        plain.setAccessibilityLabel("Fixture plain input")
        plain.setAccessibilityIdentifier("cross-app-plain-input")
        content.addSubview(plain)
        counter = NSTextField(labelWithString: "Fixture count 0")
        counter.frame = NSRect(x: 24, y: 154, width: 300, height: 26)
        counter.setAccessibilityLabel("Fixture count 0")
        content.addSubview(counter)
        let button = NSButton(title: "Increment fixture counter", target: self, action: #selector(increment))
        button.frame = NSRect(x: 24, y: 90, width: 280, height: 36)
        button.bezelStyle = .rounded
        content.addSubview(button)
        window.makeKeyAndOrderFront(nil)
        window.makeFirstResponder(plain)
        NSApp.activate(ignoringOtherApps: true)
        publish()
        timer = Timer.scheduledTimer(withTimeInterval: 0.15, repeats: true) { [weak self] _ in self?.publish() }
    }

    @objc func increment() {
        count += 1
        counter.stringValue = "Fixture count \(count)"
        counter.setAccessibilityLabel("Fixture count \(count)")
        publish()
    }

    func applicationDidBecomeActive(_ notification: Notification) { recordFocus("fixture_activated") }
    func applicationDidResignActive(_ notification: Notification) { recordFocus("fixture_deactivated") }

    func selectionLength() -> Int { plain?.currentEditor()?.selectedRange.length ?? -1 }

    func recordCommandA(keycode: UInt16, flags: UInt) {
        commandAEventSequence += 1
        let sequence = commandAEventSequence
        commandAEvents.append(["sequence": sequence, "uptime": ProcessInfo.processInfo.systemUptime,
            "keycode": Int(keycode), "modifier_flags": flags, "selection_length_before": selectionLength()])
        if commandAEvents.count > 8 {
            commandAEvents.removeFirst(commandAEvents.count - 8)
            commandAEventsDropped += 1
        }
        // Observe after normal AppKit event dispatch; never consume, rewrite
        // or synthesize an event and never invoke Select All from diagnostics.
        DispatchQueue.main.async { [weak self] in
            guard let self = self, let index = self.commandAEvents.firstIndex(where: { ($0["sequence"] as? Int) == sequence }) else { return }
            self.commandAEvents[index]["selection_length_after"] = self.selectionLength()
            self.commandAEvents[index]["observed_after_uptime"] = ProcessInfo.processInfo.systemUptime
            self.publish()
        }
    }

    func recordFocus(_ event: String, activated: NSRunningApplication? = nil) {
        let foreground = NSWorkspace.shared.frontmostApplication
        var row: [String: Any] = ["event": event, "uptime": ProcessInfo.processInfo.systemUptime,
            "fixture_active": NSApp.isActive,
            "foreground_pid": Int(foreground?.processIdentifier ?? 0),
            "foreground_bundle_id": String((foreground?.bundleIdentifier ?? "").prefix(256))]
        if let activated = activated {
            row["activated_pid"] = Int(activated.processIdentifier)
            row["activated_bundle_id"] = String((activated.bundleIdentifier ?? "").prefix(256))
        }
        focusEvents.append(row)
        if focusEvents.count > 64 {
            focusEvents.removeFirst(focusEvents.count - 64)
            focusEventsDropped += 1
        }
        if plain != nil { publish() }
    }

    func publish() {
        ticks += 1
        let value = plain.currentEditor()?.string ?? plain.stringValue
        let state: [String: Any] = ["schema_version": 1, "nonce": nonce,
            "pid": Int(ProcessInfo.processInfo.processIdentifier),
            "name": ProcessInfo.processInfo.processName,
            "bundle_id": Bundle.main.bundleIdentifier ?? "", "input": value,
            "counter": count, "ticks": ticks, "active": NSApp.isActive,
            "uptime": ProcessInfo.processInfo.systemUptime,
            "focus_events": focusEvents, "focus_events_dropped": focusEventsDropped,
            "field_editor_selection_length": selectionLength(), "command_a_events": commandAEvents,
            "command_a_events_dropped": commandAEventsDropped]
        if let data = try? JSONSerialization.data(withJSONObject: state, options: [.sortedKeys]) {
            try? data.write(to: URL(fileURLWithPath: statePath), options: [.atomic])
            try? FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: statePath)
        }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { true }
    func applicationWillTerminate(_ notification: Notification) {
        timer?.invalidate()
        if let activationObserver = activationObserver {
            NSWorkspace.shared.notificationCenter.removeObserver(activationObserver)
        }
        if let commandAMonitor = commandAMonitor { NSEvent.removeMonitor(commandAMonitor) }
    }
}

guard CommandLine.arguments.count == 3, CommandLine.arguments[1].hasPrefix("/") else { exit(2) }
let application = NSApplication.shared
application.setActivationPolicy(.regular)
let delegate = CrossAppFixture(statePath: CommandLine.arguments[1], nonce: CommandLine.arguments[2])
application.delegate = delegate
application.run()
