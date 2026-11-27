// Meko's menu bar app (macOS): the meko "m" in the menu bar. Its window lists the datapacks, this
// Meko's and the linked Meko Cloud's (filter them, copy a name, open one), searches one, and chats
// with one: each question is searched, then answered from the results by the user's own Claude Code
// (meko answers use claude). It reaches Meko's REST API at /api/v1 beside the MCP URL (plain http on
// 127.0.0.1, which Meko's port takes even under remote access) with the client token, both from
// `meko token --json`. Pages open at that same address too (Meko's web UI is on it under remote
// access as well), never at the remote one: there, with Meko Cloud sign-in, this computer's browser
// would have to sign in. web_url only says whether the web UI is on: it's "" while it's off. `meko menubar start` runs it at login; see src/meko/docs/menubar.md.
import AppKit
import SwiftUI

// MARK: - Meko's API

struct Connection: Decodable {
    let url: String  // the MCP URL, http://127.0.0.1:<port>/mcp: /api/v1 is beside it
    let token: String
    let web_url: String?
    let running: Bool?

    /// Meko on this computer: plain http on 127.0.0.1, which needs no sign-in.
    var local: String {
        var u = url
        if u.hasSuffix("/mcp") { u.removeLast(4) }
        return u
    }

    var api: String { local + "/api/v1" }
}

struct Datapack: Decodable, Identifiable, Hashable {
    let datapack_id: String
    let datapack_name: String
    let description: String?
    let web_url: String?
    var id: String { datapack_id }
}

struct Envelope<T: Decodable>: Decodable {
    let data: T?
    let error: APIError?
}

struct APIError: Decodable {
    let code: String?
    let message: String?
}

struct SearchReply: Decodable {
    struct Results: Decodable {
        let memory: [Memory]?
        let knowledge_base: [Chunk]?
        let conversation: Conversation?
    }
    struct Memory: Decodable { let id: String; let memory: String }
    struct Chunk: Decodable { let id: String; let chunk_text: String }
    struct Conversation: Decodable { let hits: [Turn]? }
    struct Turn: Decodable { let message_id: String?; let content: String? }
    let results: Results
}

struct AnswerReply: Decodable {
    let answer: String
    let cited: [Int]?
    let model: String?
}

/// A search result, and a passage an answer is written from.
struct Hit: Identifiable, Hashable {
    let id: String
    let source: String  // memory, knowledge_base or conversation, as the answer route takes them
    let text: String

    var label: String {
        switch source {
        case "memory": return "Memory"
        case "knowledge_base": return "Knowledge"
        default: return "Conversation"
        }
    }
}

enum MekoError: LocalizedError {
    case noCLI, notRunning, unreachable(String), webOff, answersOff, api(String)
    var errorDescription: String? {
        switch self {
        case .noCLI: return "Meko isn't installed: install it, then run `meko menubar start`."
        case .notRunning: return "Meko isn't running: run `meko start`."
        case .unreachable(let at): return "Meko isn't answering at \(at): run `meko status`."
        case .webOff: return "Meko's web UI is off: run `meko ui start` to open it."
        case .answersOff: return "Chat writes its answers through your own Claude Code: run `meko answers use claude` to turn it on."
        case .api(let m): return m
        }
    }
}

/// Where meko is, and its data folder: as `meko menubar start` passes them (--meko, --data-dir), or
/// else the meko beside this app (a release's bin/), or else the one `meko install` links.
struct Launch {
    var meko: String?
    var dataDir: String?

    static func parse(_ args: [String]) -> Launch {
        var l = Launch()
        var i = 1
        while i < args.count {
            let next = i + 1 < args.count ? args[i + 1] : nil
            switch args[i] {
            case "--meko": l.meko = next; i += 1
            case "--data-dir": l.dataDir = next; i += 1
            default: break
            }
            i += 1
        }
        let fm = FileManager.default
        if l.meko == nil {
            let beside = Bundle.main.bundleURL.deletingLastPathComponent().appendingPathComponent("meko").path
            let linked = fm.homeDirectoryForCurrentUser.appendingPathComponent(".local/bin/meko").path
            l.meko = [beside, linked].first { fm.isExecutableFile(atPath: $0) }
        }
        return l
    }
}

final class MekoClient: @unchecked Sendable {
    private let launch = Launch.parse(CommandLine.arguments)
    private let lock = NSLock()
    private var conn: Connection?

    /// The server's address and token, from `meko token --json`, read once and again after a 401
    /// (Meko set up again with a new token).
    func connection() throws -> Connection {
        lock.lock()
        defer { lock.unlock() }
        if let c = conn { return c }
        guard let meko = launch.meko, FileManager.default.isExecutableFile(atPath: meko) else { throw MekoError.noCLI }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: meko)
        p.arguments = ["token", "--json"] + (launch.dataDir.map { ["--data-dir", $0] } ?? [])
        let out = Pipe()
        p.standardOutput = out
        p.standardError = FileHandle.nullDevice
        try p.run()
        let data = out.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        guard p.terminationStatus == 0, let c = try? JSONDecoder().decode(Connection.self, from: data) else {
            throw MekoError.notRunning
        }
        if c.running == false { throw MekoError.notRunning }
        conn = c
        return c
    }

    private func forget() {
        lock.lock()
        conn = nil
        lock.unlock()
    }

    private func call<T: Decodable>(_ method: String, _ path: String, body: [String: Any]? = nil,
                                    timeout: TimeInterval = 30) async throws -> T {
        let c = try await Task.detached { try self.connection() }.value
        guard let url = URL(string: c.api + path) else { throw MekoError.api("Meko's address isn't a URL") }
        var req = URLRequest(url: url)
        req.httpMethod = method
        req.timeoutInterval = timeout
        req.setValue("Bearer \(c.token)", forHTTPHeaderField: "Authorization")
        if let body {
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
            req.httpBody = try JSONSerialization.data(withJSONObject: body)
        }
        let data: Data, resp: URLResponse
        do {
            (data, resp) = try await URLSession.shared.data(for: req)
        } catch {
            forget()  // Meko restarted on another port, or stopped: read the address again next time
            throw MekoError.unreachable(c.api)
        }
        let status = (resp as? HTTPURLResponse)?.statusCode ?? 0
        let env = try? JSONDecoder().decode(Envelope<T>.self, from: data)
        if let d = env?.data, (200..<300).contains(status) { return d }
        if status == 401 { forget() }
        if env?.error?.code == "ANSWERS_OFF" { throw MekoError.answersOff }
        throw MekoError.api(env?.error?.message ?? "Meko answered \(status)")
    }

    func datapacks() async throws -> [Datapack] {
        try await call("GET", "/datapacks")
    }

    func search(_ dp: Datapack, _ query: String, limit: Int = 5) async throws -> [Hit] {
        let r: SearchReply = try await call("POST", "/datapacks/\(dp.datapack_id)/search",
                                            body: ["query": query, "limit": limit, "agent_id": "meko_menubar"])
        // Taken in turn from each source, best first, as the web UI's Search answers do.
        let lists: [[Hit]] = [
            (r.results.memory ?? []).map { Hit(id: $0.id, source: "memory", text: $0.memory) },
            (r.results.knowledge_base ?? []).map { Hit(id: $0.id, source: "knowledge_base", text: $0.chunk_text) },
            (r.results.conversation?.hits ?? []).compactMap { h in
                guard let id = h.message_id, let t = h.content else { return nil }
                return Hit(id: id, source: "conversation", text: t)
            },
        ]
        var out: [Hit] = []
        var i = 0
        while lists.contains(where: { i < $0.count }) {
            for l in lists where i < l.count { out.append(l[i]) }
            i += 1
        }
        return out
    }

    /// An answer to question, written from passages (at most 10) by the harness answers use.
    func answer(_ dp: Datapack, _ question: String, _ passages: [Hit]) async throws -> AnswerReply {
        let ps = passages.prefix(10).map { ["source": $0.source, "id": $0.id, "text": $0.text] }
        return try await call("POST", "/datapacks/\(dp.datapack_id)/answer",
                              body: ["query": question, "passages": ps], timeout: 180)
    }

    /// Opens a datapack's page (its web_url: a path in Meko's web UI, or Meko Cloud's web app), or
    /// Meko's web UI with no path.
    func open(_ path: String?) throws {
        if let p = path, p.hasPrefix("http") {
            if let u = URL(string: p) { NSWorkspace.shared.open(u) }
            return
        }
        let c = try connection()
        // web_url only says the web UI is on: the page opens at this computer's own address.
        guard let web = c.web_url, !web.isEmpty else { throw MekoError.webOff }
        if let u = URL(string: c.local + (path ?? "")) { NSWorkspace.shared.open(u) }
    }
}

func copy(_ text: String) {
    NSPasteboard.general.clearContents()
    NSPasteboard.general.setString(text, forType: .string)
}

// MARK: - Model

struct ChatTurn: Identifiable {
    let id = UUID()
    let question: String
    var answer: String?
    var sources: [Hit] = []  // the passages it cites
    var error: String?
}

enum Screen: Equatable {
    case list
    case search(Datapack)
    case chat(Datapack)
}

@MainActor
final class Model: ObservableObject {
    let client = MekoClient()
    @Published var screen = Screen.list
    @Published var datapacks: [Datapack] = []
    @Published var filter = ""
    @Published var error: String?
    @Published var loading = false
    @Published var copied: String?  // what was copied last, for a moment's checkmark

    @Published var query = ""
    @Published var hits: [Hit] = []
    @Published var searching = false

    @Published var message = ""
    @Published var chats: [String: [ChatTurn]] = [:]  // by datapack, for as long as the app runs
    @Published var thinking = false
    @Published var openSources: Set<UUID> = []  // the chat turns whose sources show

    var shown: [Datapack] {
        let f = filter.trimmingCharacters(in: .whitespaces).lowercased()
        if f.isEmpty { return datapacks }
        return datapacks.filter { $0.datapack_name.lowercased().contains(f) || ($0.description ?? "").lowercased().contains(f) }
    }

    func load() async {
        loading = true
        defer { loading = false }
        do {
            datapacks = try await client.datapacks()
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }

    func open(_ path: String?) {
        do {
            try client.open(path)
        } catch {
            self.error = error.localizedDescription
        }
    }

    func show(_ s: Screen) {
        screen = s
        error = nil
        hits = []
        query = ""
    }

    func copied(_ text: String, as key: String) {
        copy(text)
        copied = key
        Task {
            try? await Task.sleep(for: .seconds(1.2))
            if copied == key { copied = nil }
        }
    }

    func search(_ dp: Datapack) async {
        let q = query.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !q.isEmpty else { return }
        searching = true
        defer { searching = false }
        do {
            hits = try await client.search(dp, q)
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }

    func send(_ dp: Datapack) async {
        let q = message.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !q.isEmpty, !thinking else { return }
        message = ""
        thinking = true
        defer { thinking = false }
        chats[dp.id, default: []].append(ChatTurn(question: q))
        var turn = ChatTurn(question: q)
        do {
            let passages = try await client.search(dp, q, limit: 8)
            let a = try await client.answer(dp, q, passages)
            turn.answer = a.answer
            turn.sources = (a.cited ?? []).compactMap { $0 >= 1 && $0 <= passages.count ? passages[$0 - 1] : nil }
        } catch {
            turn.error = error.localizedDescription
        }
        if var list = chats[dp.id], !list.isEmpty {
            list[list.count - 1] = turn
            chats[dp.id] = list
        }
    }
}

// MARK: - Views

struct RootView: View {
    @ObservedObject var model: Model

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            switch model.screen {
            case .list: ListView(model: model)
            case .search(let dp): SearchView(model: model, dp: dp)
            case .chat(let dp): ChatView(model: model, dp: dp)
            }
            if let e = model.error {
                Text(e).font(.caption).foregroundStyle(.red).lineLimit(3).textSelection(.enabled)
            }
            Divider()
            HStack {
                Button("Open Meko") { model.open(nil) }
                Spacer()
                Button("Quit") { NSApp.terminate(nil) }
            }
            .buttonStyle(.borderless)
            .font(.caption)
        }
        .padding(12)
        .frame(width: 380, height: 500)
        .task { if model.datapacks.isEmpty { await model.load() } }
    }
}

struct Header: View {
    @ObservedObject var model: Model
    let dp: Datapack

    var body: some View {
        HStack {
            Button { model.show(.list) } label: { Image(systemName: "chevron.left") }
                .buttonStyle(.borderless).help("All datapacks")
            Text(dp.datapack_name).font(.headline).lineLimit(1)
            Spacer()
            Button { model.open(dp.web_url) } label: { Image(systemName: "arrow.up.right.square") }
                .buttonStyle(.borderless).help("Open in Meko")
        }
    }
}

struct ListView: View {
    @ObservedObject var model: Model

    var body: some View {
        HStack {
            TextField("Filter datapacks", text: $model.filter)
                .textFieldStyle(.roundedBorder)
            Button { Task { await model.load() } } label: { Image(systemName: "arrow.clockwise") }
                .buttonStyle(.borderless)
                .help("Reload")
        }
        if model.loading && model.datapacks.isEmpty {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        } else {
            List(model.shown) { dp in
                HStack(spacing: 6) {
                    VStack(alignment: .leading, spacing: 1) {
                        Text(dp.datapack_name).lineLimit(1)
                        if let d = dp.description, !d.isEmpty {
                            Text(d).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                        }
                    }
                    Spacer()
                    Button { model.copied(dp.datapack_name, as: dp.id) } label: {
                        Image(systemName: model.copied == dp.id ? "checkmark" : "doc.on.doc")
                    }
                    .buttonStyle(.borderless).help("Copy name")
                    Button { model.show(.search(dp)) } label: { Image(systemName: "magnifyingglass") }
                        .buttonStyle(.borderless).help("Search")
                    Button { model.show(.chat(dp)) } label: { Image(systemName: "bubble.left.and.bubble.right") }
                        .buttonStyle(.borderless).help("Chat")
                }
                .contentShape(Rectangle())
                .onTapGesture(count: 2) { model.open(dp.web_url) }
            }
            .listStyle(.plain)
            Text("\(model.shown.count) datapacks · double-click opens one")
                .font(.caption2).foregroundStyle(.secondary)
        }
    }
}

/// A result or a source: clicking it copies its text.
struct HitRow: View {
    @ObservedObject var model: Model
    let hit: Hit

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack {
                Text(hit.label).font(.caption2).foregroundStyle(.secondary)
                Spacer()
                if model.copied == hit.id {
                    Label("Copied", systemImage: "checkmark").font(.caption2).foregroundStyle(.secondary)
                }
            }
            Text(hit.text).font(.callout).lineLimit(6)
        }
        .contentShape(Rectangle())
        .onTapGesture { model.copied(hit.text, as: hit.id) }
        .help("Click to copy")
    }
}

struct SearchView: View {
    @ObservedObject var model: Model
    let dp: Datapack
    @FocusState private var focused: Bool

    var body: some View {
        Header(model: model, dp: dp)
        TextField("Search \(dp.datapack_name)", text: $model.query)
            .textFieldStyle(.roundedBorder)
            .focused($focused)
            .onSubmit { Task { await model.search(dp) } }
            .onAppear { focused = true }
        if model.searching {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        } else {
            List(model.hits) { HitRow(model: model, hit: $0) }
                .listStyle(.plain)
            if !model.hits.isEmpty {
                Text("Click a result to copy it").font(.caption2).foregroundStyle(.secondary)
            }
        }
    }
}

struct ChatView: View {
    @ObservedObject var model: Model
    let dp: Datapack
    @FocusState private var focused: Bool

    var body: some View {
        let turns = model.chats[dp.id] ?? []
        Header(model: model, dp: dp)
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 10) {
                    if turns.isEmpty {
                        Text("Ask about what \(dp.datapack_name) holds. Each question is searched, then answered from what it finds.")
                            .font(.callout).foregroundStyle(.secondary)
                    }
                    ForEach(turns) { t in
                        TurnView(model: model, turn: t).id(t.id)
                    }
                }
                .padding(.vertical, 4)
            }
            .onChange(of: turns.count) { _ in
                if let last = turns.last { withAnimation { proxy.scrollTo(last.id, anchor: .bottom) } }
            }
        }
        HStack {
            TextField("Ask \(dp.datapack_name)", text: $model.message)
                .textFieldStyle(.roundedBorder)
                .focused($focused)
                .onSubmit { Task { await model.send(dp) } }
                .onAppear { focused = true }
            if model.thinking {
                ProgressView().controlSize(.small)
            } else if !turns.isEmpty {
                Button { model.chats[dp.id] = [] } label: { Image(systemName: "trash") }
                    .buttonStyle(.borderless).help("Clear the chat")
            }
        }
    }
}

struct TurnView: View {
    @ObservedObject var model: Model
    let turn: ChatTurn
    // In the model, not @State: this SDK's @State is a macro, and the Command Line Tools have no
    // SwiftUI macro plugin to expand it.
    private var showSources: Bool { model.openSources.contains(turn.id) }

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(turn.question)
                .font(.callout.weight(.semibold))
                .padding(6)
                .background(RoundedRectangle(cornerRadius: 6).fill(Color.accentColor.opacity(0.15)))
            if let a = turn.answer {
                Text(a).font(.callout).textSelection(.enabled)
                    .contentShape(Rectangle())
                    .onTapGesture { model.copied(a, as: turn.id.uuidString) }
                    .help("Click to copy")
                HStack {
                    if model.copied == turn.id.uuidString {
                        Label("Copied", systemImage: "checkmark")
                    }
                    if !turn.sources.isEmpty {
                        Button(showSources ? "Hide sources" : "\(turn.sources.count) sources") {
                            if showSources { model.openSources.remove(turn.id) } else { model.openSources.insert(turn.id) }
                        }
                            .buttonStyle(.borderless)
                    }
                }
                .font(.caption2).foregroundStyle(.secondary)
                if showSources {
                    ForEach(Array(turn.sources.enumerated()), id: \.offset) { _, h in
                        HitRow(model: model, hit: h).padding(.leading, 8)
                    }
                }
            } else if let e = turn.error {
                Text(e).font(.caption).foregroundStyle(.red).textSelection(.enabled)
            } else {
                Text("Searching and writing an answer…").font(.caption).foregroundStyle(.secondary)
            }
        }
    }
}

// MARK: - App

/// The meko m (m.svg, cut from the logo), a template image so macOS tints it for the menu bar.
func menuBarIcon() -> NSImage {
    let url = Bundle.main.url(forResource: "m", withExtension: "svg")
    let img = url.flatMap { NSImage(contentsOf: $0) } ?? NSImage(systemSymbolName: "m.square", accessibilityDescription: "Meko")!
    let h: CGFloat = 13
    img.size = NSSize(width: h * 340 / 230, height: h)
    img.isTemplate = true
    img.accessibilityDescription = "Meko"
    return img
}

@main
struct MekoMenuBarApp: App {
    @StateObject private var model = Model()

    var body: some Scene {
        MenuBarExtra {
            RootView(model: model)
        } label: {
            Image(nsImage: menuBarIcon())
        }
        .menuBarExtraStyle(.window)
    }
}
