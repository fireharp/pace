// PaceBar: the pace menu bar icon for macOS.
//
// It shows the state of every machine in `pace fleet` at a glance: the menu
// bar has one quiet sign while all is well and a colored capsule naming the
// machine and the resource when something needs a person, and the panel lists each machine's memory, headroom, load, jobs, biggest
// processes and active warnings. Machines that cannot be reached keep their
// last snapshot (stale), as in `pace fleet`. It only reads: it runs
// `pace fleet --json` every few seconds and never changes anything.
//
// Build and install: tray/macos/build.sh, tray/macos/install.sh.
// Headless checks: PaceBar --dump (prints the parsed view) and
// PaceBar --render out.png [fleet.json] (renders the panel to an image) and
// PaceBar --render-menubar dir (renders every menu bar style and state).

import AppKit
import SwiftUI
import UserNotifications

// MARK: - pace fleet --json

struct FleetJSON: Decodable {
    let v: Int
    let ts: Int64
    let machines: [Machine]
}

struct Machine: Decodable, Identifiable {
    let name: String
    let isSelf: Bool?
    let via: String
    let target: String?
    let state: String
    let error: String?
    let seenMs: Int64?
    let latencyMs: Int64?
    let status: PaceStatus?
    var id: String { name }

    enum CodingKeys: String, CodingKey {
        case name, isSelf = "self", via, target, state, error, seenMs, latencyMs, status
    }

    var isLive: Bool { state == "live" }
}

struct PaceStatus: Decodable {
    let ts: Int64?
    let version: String?
    let mode: String?
    let pressure: String?
    let capacityBytes: Int64?
    let availBytes: Int64?
    let headroomBytes: Int64?
    let swapUsedBytes: Int64?
    let load1Milli: Int64?
    let ncpu: Int?
    let admitting: Bool?
    let notAdmittingReasons: [String]?
    let running: [Job]?
    let queued: [Job]?
    let unmanaged: [Unit]?
    let warnings: [PaceEvent]?
    let recentAlerts: [PaceEvent]?
    let faults: [String]?

    /// Active warnings plus the recent alerts no older than `within` seconds
    /// by the daemon's own clock (so clock drift between machines does not
    /// matter). An alert that came and went between two polls still shows.
    func freshAlerts(within: Double) -> [PaceEvent] {
        let now = ts ?? Int64(Date().timeIntervalSince1970 * 1000)
        var out = warnings ?? []
        var seen = Set(out.map { $0.key })
        for e in recentAlerts ?? [] {
            guard let t = e.ts, Double(now - t) <= within * 1000, !seen.contains(e.key) else { continue }
            seen.insert(e.key)
            out.append(e)
        }
        return out
    }
}

/// How long an alert stays in the menu bar after it happened.
let alertHoldSeconds = 120.0

struct Job: Decodable {
    let id: String
    let cls: String?
    let argv: [String]?
    let usedBytes: Int64?
    let budgetBytes: Int64?
    let stage: String?
    enum CodingKeys: String, CodingKey { case id, cls = "class", argv, usedBytes, budgetBytes, stage }
}

struct Unit: Decodable {
    let comm: String?
    let nprocs: Int?
    let usedBytes: Int64?
}

struct PaceEvent: Decodable {
    let ts: Int64?
    let verb: String?
    let job: String?
    let comm: String?
    let policy: String?
    let level: String?
    let usedBytes: Int64?
    let budgetBytes: Int64?
    let next: String?
    let inMs: Int64?
    let msg: String?
    let mode: String?

    /// Identity of one alert across polls.
    var key: String { "\(ts ?? 0)|\(verb ?? "")|\(job ?? comm ?? "")|\(policy ?? "")|\(msg ?? "")" }

    /// Verbs worth a notification (INFO lines about what observe mode
    /// would do are shown, not notified).
    var notifies: Bool { ["WARN", "KILLED", "FROZEN", "THROTTLED"].contains(verb ?? "") }

    /// One readable line, e.g. "WARN mem_budget j3: 5.5G of 6.0G, kill in 20s".
    var text: String {
        var s = verb ?? "EVENT"
        if let p = policy { s += " " + p }
        if let j = job { s += " " + j } else if let c = comm { s += " " + c }
        var parts: [String] = []
        if let u = usedBytes {
            parts.append(budgetBytes.map { "\(fmtBytes(u)) of \(fmtBytes($0))" } ?? fmtBytes(u))
        }
        if let n = next {
            parts.append(inMs.map { "\(n) in \(Int($0 / 1000))s" } ?? n)
        }
        if let m = msg, !m.isEmpty { parts.append(m) }
        if let md = mode { parts.append("(\(md): not carried out)") }
        return parts.isEmpty ? s : s + ": " + parts.joined(separator: ", ")
    }
}

func decodeFleet(_ data: Data) throws -> FleetJSON {
    let d = JSONDecoder()
    d.keyDecodingStrategy = .convertFromSnakeCase
    return try d.decode(FleetJSON.self, from: data)
}

// MARK: - formatting

func fmtBytes(_ n: Int64) -> String {
    let units = ["B", "K", "M", "G", "T"]
    var v = Double(n)
    var i = 0
    while v >= 1024 && i < units.count - 1 { v /= 1024; i += 1 }
    if i == 0 { return "\(n)B" }
    return v >= 100 || i <= 2 && v >= 10 ? String(format: "%.0f%@", v, units[i]) : String(format: "%.1f%@", v, units[i])
}

func ago(_ ms: Int64?, now: Date = Date()) -> String {
    guard let ms = ms, ms > 0 else { return "never" }
    let d = now.timeIntervalSince(Date(timeIntervalSince1970: Double(ms) / 1000))
    switch d {
    case ..<2: return "now"
    case ..<60: return "\(Int(d))s ago"
    case ..<3600: return "\(Int(d / 60))m ago"
    case ..<(48 * 3600): return "\(Int(d / 3600))h ago"
    default: return "\(Int(d / 86400))d ago"
    }
}

func pressureRank(_ p: String?) -> Int {
    switch p {
    case "critical": return 2
    case "warn": return 1
    default: return 0
    }
}

/// Memory in use at which PaceBar calls it a warning.
let memWarnUsed = 0.8

/// The memory level PaceBar alerts on: 2 at critical pressure, 1 once 80 %
/// of memory is in use, else 0. The daemon's own "warn" also follows the
/// kernel's pressure level, which macOS raises with half the memory free
/// (swap filling up); that alone is shown in the panel, not as an alert.
func memRank(_ s: PaceStatus?) -> Int {
    guard let s = s else { return 0 }
    if pressureRank(s.pressure) == 2 { return 2 }
    guard let cap = s.capacityBytes, cap > 0 else { return 0 }
    return Double(cap - (s.availBytes ?? 0)) / Double(cap) >= memWarnUsed ? 1 : 0
}

/// The overall state the icon shows.
enum Overall: Equatable {
    case unknown          // no answer yet / pace not found
    case localDown        // this machine's daemon does not answer
    case pressure(Int)    // worst pressure among machines that answer
}

func overall(_ ms: [Machine]) -> Overall {
    if ms.isEmpty { return .unknown }
    if let me = ms.first(where: { $0.isSelf == true }), me.state == "pace-down" || me.state == "no-pace" {
        return .localDown
    }
    let worst = ms.filter { $0.isLive }.map { pressureRank($0.status?.pressure) }.max() ?? 0
    return .pressure(worst)
}

func pressureColor(_ p: String?) -> Color {
    switch pressureRank(p) {
    case 2: return .red
    case 1: return .orange
    default: return .green
    }
}

func stateColor(_ s: String) -> Color {
    switch s {
    case "live": return .green
    case "stale", "pace-down": return .orange
    default: return .red
    }
}

// MARK: - pace process

func findPace() -> String? {
    let env = ProcessInfo.processInfo.environment
    var candidates: [String] = []
    if let pace = env["PACE_BIN"], !pace.isEmpty { candidates.append(pace) }
    let home = NSHomeDirectory()
    candidates += ["\(home)/.local/bin/pace", "\(home)/bin/pace", "\(home)/go/bin/pace", "/opt/homebrew/bin/pace", "/usr/local/bin/pace"]
    return candidates.first { FileManager.default.isExecutableFile(atPath: $0) }
}

/// Runs pace with args, returning stdout, or an error text. Bounded by timeout.
func runPace(_ pace: String, _ args: [String], timeout: TimeInterval) -> (Data?, String?) {
    let p = Process()
    p.executableURL = URL(fileURLWithPath: pace)
    p.arguments = args
    let out = Pipe(), err = Pipe()
    p.standardOutput = out
    p.standardError = err
    p.standardInput = FileHandle.nullDevice
    do { try p.run() } catch { return (nil, "cannot run \(pace): \(error.localizedDescription)") }
    let killer = DispatchWorkItem { if p.isRunning { p.terminate() } }
    DispatchQueue.global().asyncAfter(deadline: .now() + timeout, execute: killer)
    var outData = Data(), errData = Data()
    let g = DispatchGroup()
    g.enter()
    DispatchQueue.global().async { outData = out.fileHandleForReading.readDataToEndOfFile(); g.leave() }
    g.enter()
    DispatchQueue.global().async { errData = err.fileHandleForReading.readDataToEndOfFile(); g.leave() }
    p.waitUntilExit()
    killer.cancel()
    _ = g.wait(timeout: .now() + 1)
    if p.terminationStatus != 0 {
        let e = String(data: errData, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        return (nil, "pace \(args.joined(separator: " ")) exited \(p.terminationStatus)" + (e.isEmpty ? "" : ": \(e)"))
    }
    return (outData, nil)
}

// MARK: - store

@MainActor
final class FleetStore: ObservableObject {
    @Published var machines: [Machine] = []
    @Published var updated: Date?
    @Published var problem: String?
    @Published var refreshing = false
    @Published var notify: Bool = UserDefaults.standard.object(forKey: "notify") as? Bool ?? true {
        didSet { UserDefaults.standard.set(notify, forKey: "notify"); if notify { Notifier.shared.authorize() } }
    }
    @Published var style: BarStyle = BarStyle(rawValue: UserDefaults.standard.string(forKey: "menuBarStyle") ?? "") ?? .status {
        didSet { UserDefaults.standard.set(style.rawValue, forKey: "menuBarStyle") }
    }

    let pace: String?
    let interval: TimeInterval
    private var timer: Timer?
    private var lastPressure: [String: Int] = [:]
    private var lastState: [String: String] = [:]
    private var seenWarnings: Set<String> = []
    private var first = true

    init(interval: TimeInterval = 10) {
        pace = findPace()
        self.interval = interval
        if pace == nil { problem = "pace not found (looked in ~/.local/bin, ~/bin, ~/go/bin, /opt/homebrew/bin, /usr/local/bin; set PACE_BIN)" }
    }

    func start() {
        if notify { Notifier.shared.authorize() }
        refresh()
        timer = Timer.scheduledTimer(withTimeInterval: interval, repeats: true) { [weak self] _ in
            guard let store = self else { return }
            Task { @MainActor in store.refresh() }
        }
    }

    func refresh() {
        guard let pace = pace, !refreshing else { return }
        refreshing = true
        Task.detached(priority: .utility) {
            let (data, err) = runPace(pace, ["fleet", "--json"], timeout: 30)
            var fleet: FleetJSON?
            var problem = err
            if let data = data {
                do { fleet = try decodeFleet(data) } catch { problem = "cannot read pace fleet --json: \(error)" }
            }
            await MainActor.run { [fleet, problem] in
                self.refreshing = false
                self.problem = problem
                if let f = fleet {
                    self.apply(f.machines)
                }
            }
        }
    }

    func apply(_ ms: [Machine]) {
        machines = ms
        updated = Date()
        notifyChanges(ms)
        first = false
    }

    /// Notifies on what needs a person: pressure rising to warn or critical,
    /// a daemon that stopped answering, and new governor warnings. Losing the
    /// connection to a peer is not one of them.
    private func notifyChanges(_ ms: [Machine]) {
        for m in ms {
            let rank = m.isLive ? memRank(m.status) : (lastPressure[m.name] ?? 0)
            let before = lastPressure[m.name] ?? 0
            if notify && !first && m.isLive && rank > before {
                let s = m.status
                Notifier.shared.post(title: "\(m.name): memory \(rank == 2 ? "critical" : "over 80% in use")",
                                     body: "headroom \(fmtBytes(s?.headroomBytes ?? 0)), \(fmtBytes(s?.availBytes ?? 0)) free of \(fmtBytes(s?.capacityBytes ?? 0))")
            }
            lastPressure[m.name] = rank
            let prevState = lastState[m.name]
            if notify && !first && m.state == "pace-down" && prevState != "pace-down" {
                Notifier.shared.post(title: "\(m.name): pace is not answering", body: m.error ?? "the daemon did not answer")
            }
            lastState[m.name] = m.state
            if m.isLive, let st = m.status {
                // active warnings and recent alerts: one that ended between
                // two polls is still notified, once
                for w in (st.warnings ?? []) + (st.recentAlerts ?? []) where w.notifies {
                    let key = "\(m.name)|" + w.key
                    if !seenWarnings.contains(key) {
                        seenWarnings.insert(key)
                        if notify && !first { Notifier.shared.post(title: "\(m.name): pace \(w.verb ?? "WARN")", body: w.text) }
                    }
                }
            }
        }
        if seenWarnings.count > 2000 { seenWarnings.removeAll() }
    }

    /// The menu bar image; with no pace or no answer yet, the dotted circle.
    var menuImage: NSImage {
        let img = menuBarImage(style, pace == nil ? [] : barItems(machines), dark: menuBarIsDark())
        img.accessibilityDescription = "Pace: " + barSummary(barItems(machines))
        return img
    }

    func openTerminal(_ command: String) {
        let pace = self.pace ?? "pace"
        let script = "#!/bin/sh\nclear\nexec '\(pace.replacingOccurrences(of: "'", with: "'\\''"))' \(command)\n"
        let url = FileManager.default.temporaryDirectory.appendingPathComponent("pace-\(command.split(separator: " ").first ?? "x").command")
        do {
            try script.write(to: url, atomically: true, encoding: .utf8)
            try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: url.path)
            NSWorkspace.shared.open(url)
        } catch {
            problem = "cannot open Terminal: \(error.localizedDescription)"
        }
    }
}

// MARK: - notifications

final class Notifier {
    static let shared = Notifier()
    private var allowed: Bool?

    func authorize() {
        guard Bundle.main.bundleIdentifier != nil else { return }
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound]) { ok, _ in
            self.allowed = ok
        }
    }

    func post(title: String, body: String) {
        if Bundle.main.bundleIdentifier != nil, allowed == true {
            let c = UNMutableNotificationContent()
            c.title = title
            c.body = body
            c.sound = .default
            UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: UUID().uuidString, content: c, trigger: nil))
            return
        }
        // Fallback when notifications are not allowed for the app bundle.
        let esc = { (s: String) in s.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"") }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/osascript")
        p.arguments = ["-e", "display notification \"\(esc(body))\" with title \"\(esc(title))\""]
        try? p.run()
    }
}

// MARK: - menu bar item
//
// The menu bar item is one image, drawn by hand so it looks the same on
// screen and in --render-menubar. When all is well it is one quiet sign
// (or, by setting, a memory bar or a figure per machine). Anything that
// needs a person is a filled capsule that names the machine and the
// resource: amber with dark text for a warning, red with white text for
// critical pressure, a kill and this Mac's daemon being down. The panel
// has the details.

enum BarStyle: String, CaseIterable, Identifiable {
    case status, bars, text
    var id: String { rawValue }

    var title: String {
        switch self {
        case .status: return "Status"
        case .bars: return "Bars"
        case .text: return "Text"
        }
    }

    var blurb: String {
        switch self {
        case .status: return "one sign when all is well, alerts otherwise"
        case .bars: return "a memory bar per machine, plus alerts"
        case .text: return "memory in use per machine, alerts inverted"
        }
    }
}

/// What is wrong with one machine: the resources concerned (MEM, CPU,
/// DISK, JOB), then KILL or DOWN when that is the case.
struct BarAlert: Equatable {
    var severe: Bool
    var what: [String]

    var symbol: String? {
        if what.contains("DOWN") { return "bolt.slash.fill" }
        if what.contains("KILL") { return "xmark.octagon.fill" }
        return nil // a heavy "!"
    }
    var fill: NSColor { severe ? .systemRed : .systemOrange }
    var ink: NSColor { severe ? .white : NSColor(white: 0, alpha: 0.85) }
    /// The symbol in ink, with its inner mark cut out in the fill color.
    var icon: NSImage? {
        guard let s = symbol else { return nil }
        return symbolImage(s, size: 10, colors: s == "bolt.slash.fill" ? [ink] : [fill, ink])
    }
    func label(_ name: String, multi: Bool) -> String {
        (multi ? [name] : []).appending(contentsOf: what).joined(separator: " ")
    }
}

extension Array {
    func appending(contentsOf other: [Element]) -> [Element] { self + other }
}

/// What the menu bar shows of one machine.
struct BarItem {
    var name: String
    var mem: Double = 0          // fraction of memory in use, 0...1
    var level = 0                // 0 normal, 1 warn, 2 critical: colors the glyphs
    var alert: BarAlert?
    var dim = false              // stale snapshot
    var down = false             // no data to show
}

func policyResource(_ p: String?) -> String {
    switch p {
    case "mem_budget", "pressure": return "MEM"
    case "cpu": return "CPU"
    case "disk": return "DISK"
    default: return "JOB" // stuck, wall_clock, orphaned
    }
}

/// The alert for one machine, nil when nothing needs a person. A peer that
/// cannot be reached is not an alert (its card in the panel says so); a
/// daemon that does not answer is.
func barAlert(_ m: Machine) -> BarAlert? {
    if m.state == "pace-down" || (m.isSelf == true && m.state == "no-pace") {
        return BarAlert(severe: m.isSelf == true, what: ["DOWN"])
    }
    guard m.isLive, let s = m.status else { return nil }
    var severe = false
    var what: [String] = []
    var kill = false
    func add(_ r: String) { if !what.contains(r) { what.append(r) } }
    let rank = memRank(s)
    if rank > 0 { add("MEM"); severe = severe || rank == 2 }
    for w in s.freshAlerts(within: alertHoldSeconds) {
        // A pressure warning below the 80 % mark is the kernel-level case
        // (see memRank): the panel shows it, the menu bar does not.
        let acted = w.verb == "KILLED" || w.verb == "FROZEN" || (w.next == "kill" && (w.mode ?? "").isEmpty)
        if w.policy == "pressure" && rank == 0 && !acted { continue }
        add(policyResource(w.policy))
        // KILL only when a kill happened or will happen: in observe and
        // warn-only mode (the line carries mode=) nothing is killed
        if w.verb == "KILLED" || (w.next == "kill" && (w.mode ?? "").isEmpty) { kill = true; severe = true }
        if w.verb == "FROZEN" { severe = true }
    }
    if (s.notAdmittingReasons ?? []).contains("disk") { add("DISK") }
    if what.isEmpty { return nil }
    if kill { what.append("KILL") }
    return BarAlert(severe: severe, what: Array(what.prefix(3)))
}

func barItems(_ ms: [Machine]) -> [BarItem] {
    ms.map { m in
        var it = BarItem(name: m.name)
        if let s = m.status, let cap = s.capacityBytes, cap > 0 {
            it.mem = Double(cap - (s.availBytes ?? 0)) / Double(cap)
            it.level = m.isLive ? memRank(s) : 0
        } else {
            it.down = true
        }
        it.dim = !m.isLive
        it.alert = barAlert(m)
        if it.alert?.what == ["DOWN"] { it.down = true; it.dim = false }
        return it
    }
}

struct Pal {
    let dark: Bool
    var fg: NSColor { dark ? NSColor(white: 1, alpha: 0.9) : NSColor(white: 0, alpha: 0.82) }
    var dim: NSColor { dark ? NSColor(white: 1, alpha: 0.55) : NSColor(white: 0, alpha: 0.5) }
    var track: NSColor { dark ? NSColor(white: 1, alpha: 0.2) : NSColor(white: 0, alpha: 0.16) }
    func level(_ r: Int) -> NSColor { r >= 2 ? .systemRed : r == 1 ? .systemOrange : fg }
}

let barH: CGFloat = 22

struct Piece {
    let w: CGFloat
    let draw: (CGFloat) -> Void
}

func gap(_ w: CGFloat) -> Piece { Piece(w: w) { _ in } }

func fillRound(_ r: CGRect, _ radius: CGFloat, _ c: NSColor) {
    c.setFill()
    NSBezierPath(roundedRect: r, xRadius: radius, yRadius: radius).fill()
}

func textSize(_ s: String, _ f: NSFont) -> CGSize { (s as NSString).size(withAttributes: [.font: f]) }

func drawText(_ s: String, _ f: NSFont, _ c: NSColor, x: CGFloat, baseline: CGFloat) {
    (s as NSString).draw(at: CGPoint(x: x, y: baseline + f.descender), withAttributes: [.font: f, .foregroundColor: c])
}

func symbolImage(_ name: String, size: CGFloat, colors: [NSColor]) -> NSImage? {
    let cfg = NSImage.SymbolConfiguration(pointSize: size, weight: .bold)
        .applying(NSImage.SymbolConfiguration(paletteColors: colors))
    return NSImage(systemSymbolName: name, accessibilityDescription: nil)?.withSymbolConfiguration(cfg)
}

func dimmed(_ on: Bool, _ body: () -> Void) {
    guard on, let cg = NSGraphicsContext.current?.cgContext else { body(); return }
    cg.saveGState()
    cg.setAlpha(0.4)
    body()
    cg.restoreGState()
}

/// A vertical memory bar: a track, filled from the bottom.
func memBarPiece(_ it: BarItem, _ pal: Pal, w: CGFloat) -> Piece {
    Piece(w: w) { x in
        dimmed(it.dim) {
            let h: CGFloat = 16, y = (barH - h) / 2
            fillRound(CGRect(x: x, y: y, width: w, height: h), 1.6, pal.track)
            if it.down { return }
            let fh = max(1.5, h * min(max(it.mem, 0), 1))
            NSGraphicsContext.current?.cgContext.saveGState()
            NSBezierPath(roundedRect: CGRect(x: x, y: y, width: w, height: h), xRadius: 1.6, yRadius: 1.6).addClip()
            pal.level(it.level).setFill()
            CGRect(x: x, y: y, width: w, height: fh).fill()
            NSGraphicsContext.current?.cgContext.restoreGState()
        }
    }
}

/// The all-is-well sign (or, with no machines yet, a dotted circle).
func signPiece(ok: Bool, _ pal: Pal) -> Piece {
    let img = symbolImage(ok ? "checkmark.circle" : "circle.dotted", size: 14, colors: [ok ? pal.fg : pal.dim])
    let sz = img?.size ?? NSSize(width: 16, height: 16)
    return Piece(w: sz.width) { x in
        img?.draw(in: CGRect(x: x, y: (barH - sz.height) / 2, width: sz.width, height: sz.height))
    }
}

/// The alert: a filled capsule with an icon, the machine name (when there
/// are several machines) and what is wrong.
func capsulePiece(_ it: BarItem, multi: Bool) -> Piece {
    guard let a = it.alert else { return gap(0) }
    let label = a.label(it.name, multi: multi)
    let f = NSFont.systemFont(ofSize: 10.5, weight: .heavy)
    let bang = NSFont.systemFont(ofSize: 12, weight: .black)
    let icon = a.icon
    let iw = icon?.size.width ?? textSize("!", bang).width
    let lead: CGFloat = 6, mid: CGFloat = 3, trail: CGFloat = 7
    let w = lead + iw + mid + textSize(label, f).width + trail
    return Piece(w: w) { x in
        let h: CGFloat = 17
        fillRound(CGRect(x: x, y: (barH - h) / 2, width: w, height: h), h / 2, a.fill)
        if let icon = icon {
            icon.draw(in: CGRect(x: x + lead, y: (barH - icon.size.height) / 2, width: iw, height: icon.size.height))
        } else {
            drawText("!", bang, a.ink, x: x + lead, baseline: (barH - bang.capHeight) / 2)
        }
        drawText(label, f, a.ink, x: x + lead + iw + mid, baseline: (barH - f.capHeight) / 2)
    }
}

/// Memory in use per machine (with the name above it when there are
/// several); an alert inverts it onto a filled box and names the resource.
func textPiece(_ it: BarItem, _ pal: Pal, multi: Bool) -> Piece {
    let a = it.alert
    let pct = it.down ? "down" : "\(Int((it.mem * 100).rounded()))%"
    let value = a.map { $0.what == ["DOWN"] ? "DOWN" : $0.what.joined(separator: " ") + (it.down ? "" : " " + pct) } ?? pct
    let ink = a?.ink ?? pal.fg
    let sub = a?.ink ?? pal.dim
    let nameF = NSFont.systemFont(ofSize: 6.5, weight: .bold)
    let valF = NSFont.monospacedDigitSystemFont(ofSize: multi ? 10 : 12, weight: a != nil ? .bold : multi ? .semibold : .medium)
    let bang = NSFont.systemFont(ofSize: 12, weight: .black)
    let icon = a?.icon
    let iw: CGFloat = a == nil ? 0 : (icon?.size.width ?? textSize("!", bang).width) + 3
    let tw = max(textSize(value, valF).width, multi ? textSize(it.name, nameF).width : 0)
    let pad: CGFloat = a == nil ? 0 : 5
    let w = pad + iw + tw + pad
    return Piece(w: w) { x in
        dimmed(it.dim) {
            if let a = a {
                fillRound(CGRect(x: x, y: 1.5, width: w, height: barH - 3), 5, a.fill)
                if let icon = icon {
                    icon.draw(in: CGRect(x: x + pad, y: (barH - icon.size.height) / 2, width: icon.size.width, height: icon.size.height))
                } else {
                    drawText("!", bang, ink, x: x + pad, baseline: (barH - bang.capHeight) / 2)
                }
            }
            let tx = x + pad + iw
            if multi {
                drawText(it.name, nameF, sub, x: tx, baseline: 13.6)
                drawText(value, valF, ink, x: tx, baseline: 3.8)
            } else {
                drawText(value, valF, ink, x: tx, baseline: (barH - valF.capHeight) / 2)
            }
        }
    }
}

func barPieces(_ style: BarStyle, _ items: [BarItem], _ pal: Pal) -> [Piece] {
    var out: [Piece] = []
    let multi = items.count > 1
    func sep(_ w: CGFloat) { if !out.isEmpty { out.append(gap(w)) } }
    let alerts = items.filter { $0.alert != nil }
    switch style {
    case .status:
        if alerts.isEmpty { out.append(signPiece(ok: !items.isEmpty, pal)) }
    case .bars:
        if items.isEmpty { out.append(signPiece(ok: false, pal)) }
        for it in items {
            sep(3)
            out.append(memBarPiece(it, pal, w: 7))
        }
    case .text:
        if items.isEmpty { out.append(signPiece(ok: false, pal)) }
        for it in items {
            sep(6)
            out.append(textPiece(it, pal, multi: multi))
        }
        return out
    }
    for it in alerts {
        sep(5)
        out.append(capsulePiece(it, multi: multi))
    }
    return out
}

/// Whether the menu bar is dark. It follows the wallpaper, not only the
/// system appearance, so ask the status item's own window when there is one.
@MainActor func menuBarIsDark() -> Bool {
    let w = NSApp?.windows.first { $0.className.contains("StatusBar") }
    let a = w?.effectiveAppearance ?? NSApp?.effectiveAppearance ?? NSAppearance.currentDrawing()
    return a.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua
}

/// The menu bar image for a dark or a light menu bar.
func menuBarImage(_ style: BarStyle, _ items: [BarItem], dark: Bool) -> NSImage {
    let pal = Pal(dark: dark)
    let w = max(barPieces(style, items, pal).reduce(0) { $0 + $1.w }, 8)
    let img = NSImage(size: NSSize(width: ceil(w), height: barH), flipped: false) { _ in
        var x: CGFloat = 0
        for p in barPieces(style, items, pal) {
            p.draw(x)
            x += p.w
        }
        return true
    }
    img.isTemplate = false
    return img
}

/// One line for --dump and the accessibility label: "ok" or the alerts.
func barSummary(_ items: [BarItem]) -> String {
    if items.isEmpty { return "no machines" }
    let a = items.compactMap { it in it.alert.map { ($0.severe ? "ALERT " : "warn ") + $0.label(it.name, multi: true) } }
    return a.isEmpty ? "ok" : a.joined(separator: "; ")
}

// MARK: - menu bar sheets (--render-menubar)

let mockColumns = ["all good", "memory warn", "memory critical", "disk low", "job about to be killed", "this Mac: pace down"]

/// Made-up machines for one column of the comparison sheet.
func mockItems(_ col: Int, two: Bool) -> [BarItem] {
    var mbp = BarItem(name: "mbp", mem: 0.42)
    var mini = BarItem(name: "mini", mem: 0.63)
    func set(_ a: BarAlert, mem: Double? = nil, level: Int = 0, peer: Bool = true) {
        if two && peer { mini.alert = a; mini.level = level; mini.mem = mem ?? mini.mem }
        else { mbp.alert = a; mbp.level = level; mbp.mem = mem ?? mbp.mem }
    }
    switch col {
    case 1: set(BarAlert(severe: false, what: ["MEM"]), mem: 0.84, level: 1)
    case 2: set(BarAlert(severe: true, what: ["MEM"]), mem: 0.96, level: 2)
    case 3: set(BarAlert(severe: false, what: ["DISK"]))
    case 4: set(BarAlert(severe: true, what: ["MEM", "KILL"]), mem: 0.78, peer: false)
    case 5: set(BarAlert(severe: true, what: ["DOWN"]), peer: false); mbp.down = true
    default: break
    }
    return two ? [mbp, mini] : [mbp]
}

@MainActor func renderMenuBarSheet(dark: Bool, styles: [BarStyle], to path: String) -> Bool {
    let cols = mockColumns
    let m: CGFloat = 18, labelW: CGFloat = 214, colW: CGFloat = 150, stripH: CGFloat = 24
    let top: CGFloat = 70, rowGap: CGFloat = 5, blockGap: CGFloat = 16
    let blockH = stripH * 2 + rowGap + blockGap
    let W = m + labelW + colW * CGFloat(cols.count) + m
    let H = top + blockH * CGFloat(styles.count) - blockGap + m
    guard let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: Int(W * 2), pixelsHigh: Int(H * 2),
                                     bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
                                     colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0) else { return false }
    rep.size = NSSize(width: W, height: H) // before the context: it takes its 2x scale from this
    guard let ctx = NSGraphicsContext(bitmapImageRep: rep) else { return false }
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = ctx
    NSAppearance(named: dark ? .darkAqua : .aqua)?.performAsCurrentDrawingAppearance {
        let page = dark ? NSColor(white: 0.09, alpha: 1) : NSColor(white: 0.99, alpha: 1)
        let strip = dark ? NSColor(srgbRed: 0.17, green: 0.17, blue: 0.19, alpha: 1) : NSColor(srgbRed: 0.925, green: 0.92, blue: 0.905, alpha: 1)
        let ink = dark ? NSColor(white: 1, alpha: 0.92) : NSColor(white: 0, alpha: 0.88)
        let soft = dark ? NSColor(white: 1, alpha: 0.55) : NSColor(white: 0, alpha: 0.5)
        page.setFill()
        CGRect(x: 0, y: 0, width: W, height: H).fill()
        func y(_ fromTop: CGFloat, _ h: CGFloat) -> CGFloat { H - fromTop - h }
        drawText("PaceBar in the menu bar, \(dark ? "dark" : "light")", .systemFont(ofSize: 14, weight: .bold), ink, x: m, baseline: y(m, 14) + 2)
        drawText("Real size at 2x: each strip is a 24 pt menu bar. Upper strip: one machine. Lower strip: two (mbp is this Mac, mini is a peer).",
                 .systemFont(ofSize: 9.5), soft, x: m, baseline: y(m + 20, 10) + 2)
        let hf = NSFont.systemFont(ofSize: 10, weight: .semibold)
        for (c, name) in cols.enumerated() {
            let cx = m + labelW + colW * (CGFloat(c) + 0.5)
            drawText(name, hf, soft, x: cx - textSize(name, hf).width / 2, baseline: y(top - 16, 10) + 2)
        }
        for (r, style) in styles.enumerated() {
            let t = top + blockH * CGFloat(r)
            drawText(style.title + (r == 0 ? " (default)" : ""), .systemFont(ofSize: 12, weight: .bold), ink, x: m, baseline: y(t, 12) + 2)
            drawText(style.blurb, .systemFont(ofSize: 8.5), soft, x: m, baseline: y(t + 16, 9) + 2)
            for two in [false, true] {
                let st = t + (two ? stripH + rowGap : 0)
                fillRound(CGRect(x: m + labelW, y: y(st, stripH), width: colW * CGFloat(cols.count), height: stripH), 5, strip)
                var widest: CGFloat = 0
                for c in 0..<cols.count {
                    let img = menuBarImage(style, mockItems(c, two: two), dark: dark)
                    widest = max(widest, img.size.width)
                    let cx = m + labelW + colW * (CGFloat(c) + 0.5)
                    img.draw(in: CGRect(x: (cx - img.size.width / 2).rounded(), y: y(st, stripH) + (stripH - barH) / 2, width: img.size.width, height: barH))
                }
                let normal = menuBarImage(style, mockItems(0, two: two), dark: dark).size.width
                let note = "\(two ? "2 machines" : "1 machine"): \(Int(normal)) pt, up to \(Int(widest))"
                drawText(note, .systemFont(ofSize: 8.5), soft, x: m, baseline: y(t + (two ? 42 : 30), 9) + 2)
            }
        }
    }
    NSGraphicsContext.restoreGraphicsState()
    guard let png = rep.representation(using: .png, properties: [:]) else { return false }
    return (try? png.write(to: URL(fileURLWithPath: path))) != nil
}

// MARK: - views

/// Recent alerts that are not also active warnings (the card shows those).
func recentOnly(_ s: PaceStatus) -> [PaceEvent] {
    let active = Set((s.warnings ?? []).map { $0.key })
    return (s.recentAlerts ?? []).filter { !active.contains($0.key) }
}

struct MachineCard: View {
    let m: Machine

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 6) {
                Circle().fill(stateColor(m.state)).frame(width: 8, height: 8)
                Text(m.name + (m.isSelf == true ? " (this Mac)" : "")).font(.system(size: 13, weight: .semibold))
                Spacer()
                if let s = m.status {
                    Text(s.mode ?? "-").font(.system(size: 11)).foregroundStyle(.secondary)
                    Text(s.pressure ?? "-").font(.system(size: 11, weight: .semibold)).foregroundStyle(pressureColor(s.pressure))
                }
            }
            if let s = m.status {
                let cap = max(s.capacityBytes ?? 0, 1)
                let avail = s.availBytes ?? 0
                MemoryBar(used: Double(cap - avail) / Double(cap), color: pressureColor(s.pressure))
                    .opacity(m.isLive ? 1 : 0.45)
                Text("\(fmtBytes(avail)) free of \(fmtBytes(cap)) · headroom \(fmtBytes(s.headroomBytes ?? 0))" + loadText(s))
                    .font(.system(size: 11)).foregroundStyle(.secondary)
                Text(jobsText(s)).font(.system(size: 11)).foregroundStyle(.secondary)
                if let top = topText(s) {
                    Text(top).font(.system(size: 11)).foregroundStyle(.secondary).lineLimit(1).truncationMode(.tail)
                }
                ForEach(Array((s.warnings ?? []).prefix(4).enumerated()), id: \.offset) { _, w in
                    Label(w.text, systemImage: "exclamationmark.triangle.fill")
                        .font(.system(size: 11)).foregroundStyle(.orange).lineLimit(2)
                }
                ForEach(Array(recentOnly(s).suffix(3).reversed().enumerated()), id: \.offset) { _, e in
                    Text("\(ago(e.ts)) · \(e.text)")
                        .font(.system(size: 10)).foregroundStyle(.orange.opacity(0.85)).lineLimit(1).truncationMode(.tail)
                }
                ForEach(Array((s.faults ?? []).prefix(2).enumerated()), id: \.offset) { _, f in
                    Text("fault: " + f).font(.system(size: 10)).foregroundStyle(.red).lineLimit(1)
                }
            }
            if !m.isLive {
                Text(staleText).font(.system(size: 10)).foregroundStyle(.secondary)
                    .lineLimit(3).fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(.vertical, 6)
    }

    var staleText: String {
        var why = m.error ?? m.state
        for prefix in ["daemon not answering: ", "pace status: "] where why.hasPrefix(prefix) {
            why = String(why.dropFirst(prefix.count))
        }
        switch m.state {
        case "stale": return "not reachable now, last snapshot \(ago(m.seenMs)): \(why)"
        case "unreachable": return "not reachable, never seen: \(why)"
        case "pace-down": return "pace is not answering" + (m.seenMs != nil ? " (last snapshot \(ago(m.seenMs)))" : "") + ": \(why)"
        case "no-pace": return "pace is not installed there"
        default: return why
        }
    }

    func loadText(_ s: PaceStatus) -> String {
        guard let l = s.load1Milli, let n = s.ncpu, n > 0 else { return "" }
        return String(format: " · load %.1f/%d", Double(l) / 1000, n)
    }

    func jobsText(_ s: PaceStatus) -> String {
        let r = s.running?.count ?? 0, q = s.queued?.count ?? 0
        var t = "jobs: \(r) running, \(q) queued"
        if s.admitting == false {
            t += " · not admitting" + ((s.notAdmittingReasons ?? []).isEmpty ? "" : " (\((s.notAdmittingReasons ?? []).joined(separator: ", ")))")
        }
        return t
    }

    func topText(_ s: PaceStatus) -> String? {
        var items: [(String, Int64)] = []
        for j in s.running ?? [] {
            let cmd = (j.argv?.first).map { URL(fileURLWithPath: $0).lastPathComponent } ?? j.id
            items.append(("\(j.id) \(cmd)", j.usedBytes ?? 0))
        }
        for u in s.unmanaged ?? [] { items.append((u.comm ?? "?", u.usedBytes ?? 0)) }
        let top = items.sorted { $0.1 > $1.1 }.prefix(3).map { "\($0.0) \(fmtBytes($0.1))" }
        return top.isEmpty ? nil : "top: " + top.joined(separator: ", ")
    }
}

/// The memory-in-use bar (drawn by hand: it renders the same on screen and
/// in --render, unlike ProgressView).
struct MemoryBar: View {
    let used: Double
    let color: Color

    var body: some View {
        GeometryReader { g in
            ZStack(alignment: .leading) {
                Capsule().fill(Color.secondary.opacity(0.2))
                Capsule().fill(color).frame(width: max(4, g.size.width * min(max(used, 0), 1)))
            }
        }
        .frame(height: 6)
    }
}

struct PanelView: View {
    @ObservedObject var store: FleetStore
    var forImage = false

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text("Pace").font(.system(size: 13, weight: .bold))
                Text(summary).font(.system(size: 11)).foregroundStyle(.secondary)
                Spacer()
                if !forImage {
                    Button { store.refresh() } label: {
                        Image(systemName: store.refreshing ? "arrow.triangle.2.circlepath" : "arrow.clockwise")
                    }.buttonStyle(.borderless).help("Refresh now")
                }
            }
            if let p = store.problem {
                Text(p).font(.system(size: 11)).foregroundStyle(.red).fixedSize(horizontal: false, vertical: true)
            }
            Divider()
            if store.machines.isEmpty && store.problem == nil {
                Text("Waiting for pace fleet…").font(.system(size: 11)).foregroundStyle(.secondary)
            }
            ForEach(store.machines) { m in
                MachineCard(m: m)
                Divider()
            }
            if !forImage {
                HStack(spacing: 10) {
                    Button("Details") { store.openTerminal("fleet -v --watch") }
                    Button("Live events") { store.openTerminal("watch") }
                    Button("Logs") { NSWorkspace.shared.open(URL(fileURLWithPath: NSHomeDirectory() + "/Library/Logs/pace")) }
                    Spacer()
                    Button("Quit") { NSApp.terminate(nil) }
                }.font(.system(size: 11))
                HStack(spacing: 12) {
                    Toggle("Notify", isOn: $store.notify).toggleStyle(.checkbox)
                    Spacer()
                    Picker("Menu bar", selection: $store.style) {
                        ForEach(BarStyle.allCases) { Text($0.title).tag($0) }
                    }.pickerStyle(.segmented).fixedSize().help("How Pace looks in the menu bar. Alerts always show as a colored capsule.")
                }.font(.system(size: 11))
            }
        }
        .padding(12)
        .frame(width: 380)
    }

    var summary: String {
        let n = store.machines.count
        let live = store.machines.filter { $0.isLive }.count
        var s = n == 1 ? "1 machine" : "\(n) machines"
        if live < n { s += ", \(n - live) not answering" }
        if let u = store.updated {
            let f = DateFormatter()
            f.dateFormat = "HH:mm:ss"
            s += " · \(f.string(from: u))"
        }
        return s
    }
}

// MARK: - app

@main
struct PaceBarMain {
    static func main() {
        let args = CommandLine.arguments
        if args.count >= 2 && args[1] == "--dump" { dump(); return }
        if args.count >= 3 && args[1] == "--render-menubar" { renderMenuBar(dir: args[2]); return }
        if args.count >= 3 && args[1] == "--render" { render(to: args[2], json: args.count >= 4 ? args[3] : nil); return }
        if let id = Bundle.main.bundleIdentifier,
           NSRunningApplication.runningApplications(withBundleIdentifier: id).contains(where: { $0 != NSRunningApplication.current }) {
            return // already running
        }
        PaceBarApp.main()
    }

    static func load(json: String?) -> [Machine]? {
        if let path = json {
            guard let d = FileManager.default.contents(atPath: path), let f = try? decodeFleet(d) else { return nil }
            return f.machines
        }
        guard let pace = findPace() else { FileHandle.standardError.write("pace not found\n".data(using: .utf8)!); return nil }
        let (data, err) = runPace(pace, ["fleet", "--json"], timeout: 30)
        if let e = err { FileHandle.standardError.write((e + "\n").data(using: .utf8)!); return nil }
        guard let d = data, let f = try? decodeFleet(d) else { return nil }
        return f.machines
    }

    static func dump() {
        guard let ms = load(json: nil) else { exit(1) }
        print("overall: \(overall(ms))")
        print("menu bar: \(barSummary(barItems(ms)))")
        for m in ms {
            let s = m.status
            print("\(m.name)\(m.isSelf == true ? "*" : "") \(m.state) mode=\(s?.mode ?? "-") pressure=\(s?.pressure ?? "-") avail=\(fmtBytes(s?.availBytes ?? 0)) headroom=\(fmtBytes(s?.headroomBytes ?? 0)) seen=\(ago(m.seenMs)) warnings=\(s?.warnings?.count ?? 0) recent_alerts=\(s?.recentAlerts?.count ?? 0)\(m.error.map { " error=\($0)" } ?? "")")
        }
    }

    /// Writes the comparison sheets of every menu bar style: dir/menubar-light.png and dir/menubar-dark.png.
    @MainActor static func renderMenuBar(dir: String) {
        try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        for dark in [false, true] {
            let path = "\(dir)/menubar-\(dark ? "dark" : "light").png"
            guard renderMenuBarSheet(dark: dark, styles: BarStyle.allCases, to: path) else { exit(1) }
            print(path)
        }
    }

    @MainActor static func render(to path: String, json: String?) {
        guard let ms = load(json: json) else { exit(1) }
        let store = FleetStore()
        store.apply(ms)
        let view = PanelView(store: store, forImage: true).background(Color(nsColor: .windowBackgroundColor))
        let r = ImageRenderer(content: view)
        r.scale = 2
        guard let img = r.nsImage, let tiff = img.tiffRepresentation,
              let png = NSBitmapImageRep(data: tiff)?.representation(using: .png, properties: [:]) else { exit(1) }
        do { try png.write(to: URL(fileURLWithPath: path)) } catch { exit(1) }
        // the menu bar icon for the same state, next to it
        let icon = menuBarImage(store.style, barItems(ms), dark: false)
        if let t = icon.tiffRepresentation, let p = NSBitmapImageRep(data: t)?.representation(using: .png, properties: [:]) {
            try? p.write(to: URL(fileURLWithPath: path.replacingOccurrences(of: ".png", with: "-icon.png")))
        }
        print("overall: \(overall(ms))")
    }
}

struct PaceBarApp: App {
    @StateObject private var store: FleetStore = {
        let s = FleetStore()
        s.start()
        return s
    }()

    var body: some Scene {
        MenuBarExtra {
            PanelView(store: store)
        } label: {
            Image(nsImage: store.menuImage)
        }
        .menuBarExtraStyle(.window)
    }
}
