import JingClawKit
import SwiftUI

/// The window: sessions on the left, the conversation on the right.
struct SessionView: View {
    @Bindable var store: SessionStore
    @State private var draft = ""

    var body: some View {
        NavigationSplitView {
            SessionList(store: store)
                .navigationSplitViewColumnWidth(min: 200, ideal: 240)
        } detail: {
            VStack(spacing: 0) {
                Timeline(state: store.state)

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

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 12) {
                    ForEach(Array(state.messages.enumerated()), id: \.offset) { index, message in
                        MessageRow(message: message).id(index)
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

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(message.role == .user ? "you" : "agent")
                .font(.caption).bold()
                .foregroundStyle(message.role == .user ? .blue : .secondary)

            if !message.text.isEmpty {
                Text(message.text)
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }

            ForEach(Array(message.toolCalls.enumerated()), id: \.offset) { _, call in
                HStack(spacing: 6) {
                    Image(systemName: icon(for: call))
                        .foregroundStyle(call.isError ? .red : .secondary)
                    Text(call.name).font(.system(.caption, design: .monospaced))
                }
            }
        }
    }

    /// A finished call and a failed one must not look the same: a client that
    /// draws both the same way tells somebody the work was done.
    private func icon(for call: ToolCall) -> String {
        if call.isError { return "xmark.circle" }
        return call.completed ? "checkmark.circle" : "circle.dotted"
    }
}

private struct Approvals: View {
    @Bindable var store: SessionStore

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(store.state.pendingApprovals, id: \.self) { id in
                HStack {
                    Text("Waiting for a decision").font(.callout)
                    Text(id).font(.system(.caption, design: .monospaced))
                        .foregroundStyle(.secondary)
                    Spacer()
                    Button("Deny") { Task { await store.decide(id, allow: false) } }
                    Button("Allow") { Task { await store.decide(id, allow: true) } }
                        .keyboardShortcut(.defaultAction)
                }
            }
        }
        .padding(12)
        .background(.quaternary)
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
