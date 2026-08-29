import JingClawKit
import SwiftUI

/// The window: sessions on the left, the conversation on the right.
struct SessionView: View {
    @Bindable var store: SessionStore
    @Bindable var gateway: GatewayStore

    @State private var draft = ""
    @State private var showingGateway = false

    var body: some View {
        NavigationSplitView {
            SessionList(store: store)
                .navigationSplitViewColumnWidth(min: 200, ideal: 240)
        } detail: {
            VStack(spacing: 0) {
                Timeline(state: store.state, store: store)

                if !store.state.pendingApprovals.isEmpty {
                    Approvals(store: store)
                }

                Composer(draft: $draft, store: store)
            }
            .toolbar {
                ToolbarItem(placement: .status) {
                    if !store.state.activeRun.isEmpty {
                        HStack(spacing: 6) {
                            ProgressView().controlSize(.small)
                            Button("Stop") { Task { await store.interrupt() } }
                        }
                    }
                }

                ToolbarItem {
                    Button {
                        showingGateway = true
                    } label: {
                        Label("Channels", systemImage: "bubble.left.and.bubble.right")
                    }
                    .help("Which chat channels may reach this agent")
                }
            }
            .sheet(isPresented: $showingGateway) {
                GatewayPane(gateway: gateway)
            }
        }
        .overlay(alignment: .bottom) {
            if !store.status.isEmpty {
                Text(store.status)
                    .font(.caption)
                    .padding(6)
                    .background(.thinMaterial, in: Capsule())
                    .padding(8)
            }
        }
    }
}

private struct SessionList: View {
    @Bindable var store: SessionStore

    var body: some View {
        List(store.sessions, selection: selection) { session in
            Text(session.title?.isEmpty == false ? session.title! : session.id)
                .lineLimit(1)
                .tag(session.id)
        }
        .toolbar {
            ToolbarItem {
                Button {
                    Task { await store.newSession(title: "New session") }
                } label: {
                    Label("New", systemImage: "plus")
                }
            }
        }
        .task { await store.loadSessions() }
    }

    private var selection: Binding<String?> {
        Binding(
            get: { store.selected },
            set: { id in if let id { Task { await store.open(id) } } }
        )
    }
}

private struct Timeline: View {
    let state: SessionState
    @Bindable var store: SessionStore

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 12) {
                    ForEach(Array(state.messages.enumerated()), id: \.offset) { index, message in
                        MessageRow(message: message, store: store).id(index)
                    }
                }
                .padding(16)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .onChange(of: state.messages.count) {
                withAnimation { proxy.scrollTo(state.messages.count - 1, anchor: .bottom) }
            }
        }
    }
}

private struct MessageRow: View {
    let message: Message
    @Bindable var store: SessionStore

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(message.role == .user ? "you" : "agent")
                .font(.caption).bold()
                .foregroundStyle(message.role == .user ? .blue : .secondary)

            // The working-out, set apart because it is not the answer.
            // Dimmed and inset so a glance down the transcript reads the
            // replies and skips these.
            if !message.reasoning.isEmpty {
                Text(message.reasoning)
                    .font(.callout.italic())
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
                    .padding(.leading, 8)
                    .overlay(alignment: .leading) {
                        Rectangle().frame(width: 2).foregroundStyle(.quaternary)
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
            }

            if !message.text.isEmpty {
                Text(message.text)
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }

            ForEach(Array(message.toolCalls.enumerated()), id: \.offset) { _, call in
                ToolCallRow(call: call, store: store)
            }
        }
    }

}

/// One tool a turn asked for, and the stored output it produced.
private struct ToolCallRow: View {
    let call: ToolCall
    @Bindable var store: SessionStore

    @State private var saving = false

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 6) {
                Image(systemName: icon)
                    .foregroundStyle(call.isError ? .red : .secondary)
                Text(call.name).font(.system(.caption, design: .monospaced))

                // Stored output is shown where the reader already is rather
                // than in a window they have to go and find: what somebody
                // wants with a build log is to glance at it next to the line
                // that produced it.
                if !call.artifact.isEmpty {
                    Button(store.opened[call.artifact] == nil ? "Show output" : "Hide") {
                        if store.opened[call.artifact] == nil {
                            Task { await store.openArtifact(call.artifact) }
                        } else {
                            store.closeArtifact(call.artifact)
                        }
                    }
                    .buttonStyle(.link)
                    .font(.caption)

                    Button("Save…") { save() }
                        .buttonStyle(.link)
                        .font(.caption)
                        .disabled(saving)
                }
            }

            if let output = store.opened[call.artifact], !call.artifact.isEmpty {
                ScrollView {
                    Text(output)
                        .font(.system(.caption, design: .monospaced))
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(8)
                }
                .frame(maxHeight: 260)
                .background(.quaternary.opacity(0.4), in: RoundedRectangle(cornerRadius: 6))
            }
        }
    }

    /// A finished call and a failed one must not look the same: a client that
    /// draws both the same way tells somebody the work was done.
    private var icon: String {
        if call.isError { return "xmark.circle" }
        return call.completed ? "checkmark.circle" : "circle.dotted"
    }

    private func save() {
        let panel = NSSavePanel()
        panel.nameFieldStringValue = call.artifact + ".txt"
        // Asked for rather than written somewhere chosen here: this is the
        // user's disk, and a file that appears in a folder nobody picked is
        // one nobody finds.
        guard panel.runModal() == .OK, let url = panel.url else { return }

        saving = true
        Task {
            await store.saveArtifact(call.artifact, to: url)
            saving = false
        }
    }
}

private struct Approvals: View {
    @Bindable var store: SessionStore

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            ForEach(store.state.pendingApprovals, id: \.self) { id in
                ApprovalRow(id: id, asking: asking(id), store: store)
            }
        }
        .padding(12)
        .background(.quaternary)
    }

    /// What this approval is asking for, once the details have arrived.
    ///
    /// The id comes from the reducer and the rest is fetched, so for a moment
    /// there is an id and nothing else. Shown as an id rather than withheld:
    /// a run is paused, and saying so late is worse than saying so plainly.
    private func asking(_ id: String) -> PendingApproval? {
        store.waiting.first { $0.id == id }
    }
}

private struct ApprovalRow: View {
    let id: String
    let asking: PendingApproval?
    @Bindable var store: SessionStore

    @State private var showingArguments = false

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text(asking?.toolName.isEmpty == false ? asking!.toolName : "Waiting for a decision")
                    .font(.system(.callout, design: .monospaced)).bold()
                Spacer()
                Button("Deny") { Task { await store.decide(id, allow: false) } }
                Button("Allow") { Task { await store.decide(id, allow: true) } }
                    .keyboardShortcut(.defaultAction)
            }

            if let effects = asking?.effects, !effects.isEmpty {
                Text(effects.joined(separator: " · "))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }

            // The call as the daemon rendered it: a diff for an edit, the
            // command line for an execution. Coloured by line, because a
            // change is what a person most often has to review and a wall of
            // one colour is a wall nobody reads.
            if let preview = asking?.preview, !preview.isEmpty {
                ScrollView {
                    VStack(alignment: .leading, spacing: 0) {
                        ForEach(Array(preview.split(separator: "\n", omittingEmptySubsequences: false)
                            .enumerated()), id: \.offset) { _, line in
                            Text(String(line))
                                .font(.system(.caption, design: .monospaced))
                                .foregroundStyle(colour(of: String(line)))
                                .frame(maxWidth: .infinity, alignment: .leading)
                        }
                    }
                    .padding(8)
                    .textSelection(.enabled)
                }
                .frame(maxHeight: 220)
                .background(.background.opacity(0.5), in: RoundedRectangle(cornerRadius: 6))

                // A preview is a rendering, and whoever allows it is allowing
                // the call underneath. Hidden by default because the point of
                // the preview is that the arguments are unreadable; reachable
                // because a rendering that disagreed with them is exactly what
                // somebody would want to catch.
                Button(showingArguments ? "Hide the exact arguments" : "Show the exact arguments") {
                    showingArguments.toggle()
                }
                .buttonStyle(.link)
                .font(.caption)
            }

            if showingArguments || (asking?.preview.isEmpty ?? true) {
                Text(asking?.arguments ?? id)
                    .font(.system(.caption, design: .monospaced))
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }

    private func colour(of line: String) -> Color {
        if line.hasPrefix("+") { return .green }
        if line.hasPrefix("-") { return .red }
        if line.hasPrefix("@@") { return .secondary }
        return .primary
    }
}

private struct Composer: View {
    @Binding var draft: String
    @Bindable var store: SessionStore

    var body: some View {
        HStack(spacing: 8) {
            TextField("Say something", text: $draft, axis: .vertical)
                .lineLimit(1...6)
                .textFieldStyle(.roundedBorder)
                .onSubmit(send)

            Button("Send", action: send)
                .keyboardShortcut(.return, modifiers: .command)
                .disabled(store.selected == nil || draft.trimmingCharacters(in: .whitespaces).isEmpty)
        }
        .padding(12)
    }

    private func send() {
        let text = draft.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty else { return }
        draft = ""
        Task { await store.send(text) }
    }
}

/// What can reach this agent from a chat platform, and what it may do there.
///
/// Read-only apart from unbinding. Adding a binding means choosing a workspace
/// and a set of people, which the configuration file describes better than a
/// dialog can and which is applied when the daemon starts; what this is for is
/// seeing what is in force right now, and being able to take one away without
/// editing a file and restarting.
private struct GatewayPane: View {
    @Bindable var gateway: GatewayStore
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Text("Channels").font(.title3).bold()
                Spacer()
                Button("Done") { dismiss() }.keyboardShortcut(.defaultAction)
            }

            if !gateway.loaded {
                // An empty list when the daemon could not be asked would say
                // "nobody can reach this agent" when the truth is "I do not
                // know", which is the more dangerous of the two to believe.
                Text(gateway.status.isEmpty ? "Reading…" : gateway.status)
                    .foregroundStyle(.secondary)
            } else if gateway.bindings.isEmpty {
                Text("Nothing is bound, so no channel can reach this agent.")
                    .foregroundStyle(.secondary)
            } else {
                List(gateway.bindings) { binding in
                    BindingRow(binding: binding, gateway: gateway)
                }
                .listStyle(.inset)
            }

            if gateway.loaded && !gateway.status.isEmpty {
                Text(gateway.status).font(.caption).foregroundStyle(.secondary)
            }
        }
        .padding(16)
        .frame(minWidth: 560, minHeight: 360)
        .task { await gateway.load() }
    }
}

private struct BindingRow: View {
    let binding: ChannelBinding
    @Bindable var gateway: GatewayStore

    @State private var confirming = false

    var body: some View {
        HStack(alignment: .top) {
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 6) {
                    Text(binding.platform).font(.caption).foregroundStyle(.secondary)
                    Text(binding.channelID).font(.system(.body, design: .monospaced))
                }

                // The profile is the whole of what this channel may do, so it
                // is said in words rather than left as a name somebody has to
                // remember the meaning of.
                Text(explain(binding.permissionProfile))
                    .font(.caption)
                    .foregroundStyle(.secondary)

                Text(who).font(.caption).foregroundStyle(.secondary)
            }

            Spacer()

            Button("Unbind", role: .destructive) { confirming = true }
                .confirmationDialog(
                    "Stop this channel reaching the agent?",
                    isPresented: $confirming
                ) {
                    Button("Unbind", role: .destructive) {
                        Task { await gateway.unbind(binding.id) }
                    }
                } message: {
                    Text("The configuration file will bind it again when the daemon restarts, "
                         + "unless you remove it there too.")
                }
        }
        .padding(.vertical, 4)
    }

    /// Who may trigger work here. Empty means nobody, which is worth saying
    /// rather than leaving as a blank line somebody reads as "everybody".
    private var who: String {
        if binding.allowedPrincipals.isEmpty {
            return "nobody is permitted to trigger work here"
        }
        return "\(binding.allowedPrincipals.count) permitted"
            + (binding.workspaceID.isEmpty ? "" : " · workspace \(binding.workspaceID)")
    }

    private func explain(_ profile: String) -> String {
        switch profile {
        case "console":
            return "a private console: reads and fetches unattended, answers its own approvals, "
                + "cannot run programs"
        case "gateway":
            return "a room other people can type in: reading is unattended, changes stop and ask, "
                + "cannot run programs"
        default:
            return profile.isEmpty ? "no profile named" : profile
        }
    }
}
