# ADR-0026: Owner-approved OAuth for project MCP clients

- Status: accepted
- Date: 2026-08-06
- Deciders: project maintainers
- Supersedes: [ADR-0014](0014-project-credential-delivery.md)
- Amends: [ADR-0012](0012-codex-integration-contract.md), [ADR-0025](0025-native-windows-admin-and-complete-uninstall.md)

## Context

The process-scoped bearer mechanism proved the project gateway, but it made LCTK configure and launch an editor. That is the wrong product boundary. LCTK owns the MCP endpoint and its authorization; an IDE already knows how to configure a Streamable HTTP server, start OAuth, store credentials, and refresh them.

The current MCP authorization specification requires protected-resource discovery, authorization-server metadata, authorization code with S256 PKCE, and exact resource audience binding. Current Codex clients support Streamable HTTP OAuth and start it themselves through **Authenticate** or `codex mcp login`. Codex also interoperates with an unknown local authorization server through OAuth Dynamic Client Registration.

## Decision

LCTK is a loopback OAuth authorization server for its project MCP resources. It never launches an IDE, edits an IDE configuration file, injects an environment variable, or displays a bearer token.

For each project, the native administrator shows the exact MCP URL and client-side setup instructions. The user adds that URL to an IDE. The IDE independently contacts the route and receives `401 Unauthorized` with OAuth Protected Resource Metadata. It performs discovery, registers as a public client, and starts authorization code flow with S256 PKCE.

The authorization request becomes a pending connection in the native LCTK window. The window displays the client name, exact callback host, requested project, scope, and expiry. Only the machine owner may approve or deny it through the independently authenticated Admin API. The browser page used by the IDE contains no administrator controls; it only waits for the native decision and then returns the result to the IDE callback.

Dynamic client registration is supported for local-client interoperability. Registered redirect URIs must use HTTPS or an HTTP loopback host, and the authorization request must match a registered URI exactly. LCTK does not advertise Client ID Metadata Documents because a completely local desktop client cannot provide the public HTTPS metadata identity that mechanism requires.

An approved request produces a single-use, short-lived authorization code bound to the client, redirect URI, project resource, and PKCE challenge. The token endpoint issues opaque short-lived access tokens and rotating refresh tokens. Only token hashes are persisted. Access tokens are accepted only by the exact project resource for which they were issued. Revocation invalidates the refresh token and every access token in the authorization.

The OAuth server and MCP resource are loopback-only. Plain HTTP is used only on loopback; no endpoint is exposed to the network. The native Admin API session remains structurally separate and cannot be obtained with an OAuth project token.

No grant is created when a project is registered. Authorization exists only after a client requests access and the owner approves it.

The superseded `grants.json` file is not migrated. After the OAuth authority opens successfully, LCTK removes that obsolete recoverable-token store so the old secrets do not remain on disk.

## Alternatives considered

### A custom first MCP `auth` tool

Rejected. An unauthenticated client cannot safely call a protected tool, and IDEs already implement the standard OAuth discovery and login flow.

### Keep launching an editor with a bearer environment variable

Rejected. It makes LCTK own editor process lifecycle, leaves user-started IDEs unauthenticated, and requires recoverable long-lived tokens.

### Edit Codex configuration automatically

Rejected. The configuration belongs to Codex and is shared by its app, CLI, and IDE extension. LCTK provides instructions and an endpoint; the client owns its own configuration.

### Client ID Metadata Documents

Rejected for this local authorization server. The mechanism requires an HTTPS client identifier whose document is hosted by the client. Dynamic registration is a standard supported mechanism and matches current Codex behavior without introducing an Internet-hosted identity service.

## Consequences

- A user can start any compatible IDE normally and authorize it without copying a secret.
- A repository registration alone grants no client access.
- Revocation and refresh no longer require restarting an editor.
- The authorization surface is larger than a static bearer check and therefore has explicit expiry, exact binding, rotation, validation, and owner-approval tests.
- The old `grant show --reveal`, `codex env`, `codex config --apply`, and `codex launch` contracts are removed rather than retained as compatibility paths.
