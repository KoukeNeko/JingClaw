import Foundation
import Observation

/// One channel that may reach this agent.
///
/// Named ChannelBinding rather than Binding because SwiftUI already has a
/// Binding and a view file importing both would have to say which every time.
///
/// A binding is the whole of who can talk to the machine through a chat
/// platform, so it is the thing an operator most wants to be able to see
/// without reading a configuration file — and the thing they most want to be
/// able to remove in a hurry.
public struct ChannelBinding: Identifiable, Equatable, Sendable, Decodable {
    public var id: String
    public var platform: String
    public var accountID: String
    public var tenantID: String
    public var channelID: String
    public var workspaceID: String

    /// Which set of rules applies. "gateway" is a room other people can type
    /// in; "console" is a private channel an operator controls, which may
    /// answer its own approvals. Neither can run programs.
    public var permissionProfile: String

    /// Who may trigger work here. Empty means nobody, which is the right
    /// default for a room and worth showing plainly rather than as a blank.
    public var allowedPrincipals: [String]

    private enum CodingKeys: String, CodingKey {
        case id
        case platform
        case accountID = "accountId"
        case tenantID = "tenantId"
        case channelID = "channelId"
        case workspaceID = "workspaceId"
        case permissionProfile
        case allowedPrincipals
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        id = try container.decode(String.self, forKey: .id)
        platform = try container.decodeIfPresent(String.self, forKey: .platform) ?? ""
        accountID = try container.decodeIfPresent(String.self, forKey: .accountID) ?? ""
        tenantID = try container.decodeIfPresent(String.self, forKey: .tenantID) ?? ""
        channelID = try container.decodeIfPresent(String.self, forKey: .channelID) ?? ""
        workspaceID = try container.decodeIfPresent(String.self, forKey: .workspaceID) ?? ""
        permissionProfile =
            try container.decodeIfPresent(String.self, forKey: .permissionProfile) ?? ""
        allowedPrincipals =
            try container.decodeIfPresent([String].self, forKey: .allowedPrincipals) ?? []
    }
}

private struct BindingListResponse: Decodable {
    var bindings: [ChannelBinding]?
}

/// What the gateway plane looks like from the machine.
///
/// Read-only apart from unbinding, deliberately. Adding a binding means
/// choosing a workspace and a set of people, which the configuration file
/// describes better than a dialog can and which is applied at startup; what a
/// window is for is seeing what is in force now, and being able to take one
/// away without editing a file and restarting.
@Observable
@MainActor
public final class GatewayStore {
    public private(set) var bindings: [ChannelBinding] = []
    public private(set) var status = ""

    /// Whether the daemon answered at all. A window that shows an empty list
    /// when it could not ask is one that says "nobody can reach this agent"
    /// when the truth is "I do not know".
    public private(set) var loaded = false

    private let client: DaemonClient

    public init(client: DaemonClient) {
        self.client = client
    }

    public func load() async {
        do {
            let listed: BindingListResponse = try await client.call(
                "ChannelService", "ListBindings")
            bindings = listed.bindings ?? []
            loaded = true
            status = ""
        } catch {
            loaded = false
            status = "could not read the bindings: \(error)"
        }
    }

    /// Removes a binding, so that channel can no longer reach the agent.
    public func unbind(_ id: String) async {
        do {
            try await client.send(
                "ChannelService", "DeleteBinding",
                ["meta": ["clientId": DaemonClient.clientName], "bindingId": id])
            bindings.removeAll { $0.id == id }
            status = "unbound"
        } catch {
            status = "could not unbind: \(error)"
        }
    }
}
