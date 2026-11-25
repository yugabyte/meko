// Meko menu bar POC: the meko "m" in the macOS menu bar. Its window lists the
// datapacks (filter, copy a name, open one) and runs a quick search in one.
// It talks to the local Meko's REST API with the client token from `meko token --json`.
import AppKit
import SwiftUI

// MARK: - Meko client

struct Connection: Decodable {
    let token: String
    let web_url: String
    let running: Bool?
}

struct Datapack: Decodable, Identifiable, Hashable {
    let datapack_id: String
    let datapack_name: String
    let description: String?
    let grant: String?
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
    }
    struct Memory: Decodable { let id: String; let memory: String; let score: Double? }
    struct Chunk: Decodable { let id: String; let chunk_text: String }
    let results: Results
    let match_quality: String?
}

struct Hit: Identifiable {
    let id: String
    let source: String
    let text: String
}

enum MekoError: LocalizedError {
    case noCLI, notRunning, api(String)
    var errorDescription: String? {
        switch self {
        case .noCLI: return "Meko isn't installed. Install it, then reopen this menu."
        case .notRunning: return "Meko isn't running. Run `meko start`."
        case .api(let m): return m
        }
    }
}

final class MekoClient {
    private var conn: Connection?

    // The `meko` CLI is the stable way to find the server and its token.
    private func mekoPath() -> String? {
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        let candidates = ["\(home)/.local/bin/meko", "/opt/homebrew/bin/meko", "/usr/local/bin/meko"]
        return candidates.first { FileManager.default.isExecutableFile(atPath: $0) }
    }

    func connection(refresh: Bool = false) throws -> Connection {
        if let c = conn, !refresh { return c }
        guard let path = mekoPath() else { throw MekoError.noCLI }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: path)
        p.arguments = ["token", "--json"]
        let out = Pipe()
        p.standardOutput = out
        p.standardError = Pipe()
        try p.run()
        let data = out.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        let c = try JSONDecoder().decode(Connection.self, from: data)
        if c.running == false { throw MekoError.notRunning }
        conn = c
        return c
    }

    private func call<T: Decodable>(_ method: String, _ path: String, body: [String: Any]? = nil) async throws -> T {
        let c = try connection()
        var req = URLRequest(url: URL(string: c.web_url + "/api/v1" + path)!)
        req.httpMethod = method
        req.timeoutInterval = 20
        req.setValue("Bearer \(c.token)", forHTTPHeaderField: "Authorization")
        if let body {
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
            req.httpBody = try JSONSerialization.data(withJSONObject: body)
        }
        let (data, resp) = try await URLSession.shared.data(for: req)
        let env = try JSONDecoder().decode(Envelope<T>.self, from: data)
        if let d = env.data { return d }
        let status = (resp as? HTTPURLResponse)?.statusCode ?? 0
        if status == 401 { conn = nil }  // token changed (Meko destroyed and set up again): read it again next time
        throw MekoError.api(env.error?.message ?? "Meko answered \(status)")
    }

    func datapacks() async throws -> [Datapack] {
        try await call("GET", "/datapacks")
    }

    func search(_ dp: Datapack, _ query: String) async throws -> [Hit] {
        let r: SearchReply = try await call("POST", "/datapacks/\(dp.datapack_id)/search",
                                            body: ["query": query, "limit": 5, "agent_id": "meko_menubar"])
        let mem = (r.results.memory ?? []).map { Hit(id: $0.id, source: "Memory", text: $0.memory) }
        let kb = (r.results.knowledge_base ?? []).map { Hit(id: $0.id, source: "Knowledge", text: $0.chunk_text) }
        return mem + kb
    }

    func open(_ dp: Datapack) {
        guard let c = conn, let w = dp.web_url else { return }
        let s = w.hasPrefix("http") ? w : c.web_url + w
        if let u = URL(string: s) { NSWorkspace.shared.open(u) }
    }
}

// MARK: - Model

@MainActor
final class Model: ObservableObject {
    let client = MekoClient()
    @Published var datapacks: [Datapack] = []
    @Published var filter = ""
    @Published var error: String?
    @Published var loading = false
    @Published var selected: Datapack?
    @Published var query = ""
    @Published var hits: [Hit] = []
    @Published var searching = false
    @Published var copied: String?

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

    func copy(_ dp: Datapack) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(dp.datapack_name, forType: .string)
        copied = dp.id
        Task { try? await Task.sleep(for: .seconds(1.2)); if copied == dp.id { copied = nil } }
    }

    func search() async {
        guard let dp = selected, !query.trimmingCharacters(in: .whitespaces).isEmpty else { return }
        searching = true
        defer { searching = false }
        do {
            hits = try await client.search(dp, query)
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }
}

// MARK: - Views

struct RootView: View {
    @ObservedObject var model: Model

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if let dp = model.selected {
                SearchView(model: model, dp: dp)
            } else {
                ListView(model: model)
            }
            if let e = model.error {
                Text(e).font(.caption).foregroundStyle(.red).lineLimit(3)
            }
            Divider()
            HStack {
                Button("Open Meko") { if let c = try? model.client.connection(), let u = URL(string: c.web_url) { NSWorkspace.shared.open(u) } }
                Spacer()
                Button("Quit") { NSApp.terminate(nil) }
            }
            .buttonStyle(.borderless)
            .font(.caption)
        }
        .padding(12)
        .frame(width: 360, height: 460)
        .task { if model.datapacks.isEmpty { await model.load() } }
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
                    Button { model.copy(dp) } label: {
                        Image(systemName: model.copied == dp.id ? "checkmark" : "doc.on.doc")
                    }
                    .buttonStyle(.borderless).help("Copy name")
                    Button { model.selected = dp; model.hits = []; model.query = "" } label: {
                        Image(systemName: "magnifyingglass")
                    }
                    .buttonStyle(.borderless).help("Search")
                }
                .contentShape(Rectangle())
                .onTapGesture(count: 2) { model.client.open(dp) }
            }
            .listStyle(.plain)
            Text("\(model.shown.count) datapacks · double-click opens one")
                .font(.caption2).foregroundStyle(.secondary)
        }
    }
}

struct SearchView: View {
    @ObservedObject var model: Model
    let dp: Datapack
    @FocusState private var focused: Bool

    var body: some View {
        HStack {
            Button { model.selected = nil } label: { Image(systemName: "chevron.left") }
                .buttonStyle(.borderless)
            Text(dp.datapack_name).font(.headline).lineLimit(1)
            Spacer()
            Button { model.client.open(dp) } label: { Image(systemName: "arrow.up.right.square") }
                .buttonStyle(.borderless).help("Open in Meko")
        }
        TextField("Search \(dp.datapack_name)", text: $model.query)
            .textFieldStyle(.roundedBorder)
            .focused($focused)
            .onSubmit { Task { await model.search() } }
            .onAppear { focused = true }
        if model.searching {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        } else {
            List(model.hits) { h in
                VStack(alignment: .leading, spacing: 2) {
                    Text(h.source).font(.caption2).foregroundStyle(.secondary)
                    Text(h.text).font(.callout).lineLimit(6).textSelection(.enabled)
                }
                .contextMenu {
                    Button("Copy") {
                        NSPasteboard.general.clearContents()
                        NSPasteboard.general.setString(h.text, forType: .string)
                    }
                }
            }
            .listStyle(.plain)
        }
    }
}

// MARK: - App

func menuBarIcon() -> NSImage {
    // m.svg sits next to the binary in Contents/Resources; as a template image macOS tints it for light and dark menu bars.
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
