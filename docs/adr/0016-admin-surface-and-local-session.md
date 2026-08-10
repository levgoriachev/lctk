# ADR-0016: The admin surface and its local session

- Status: accepted
- Date: 2026-08-02
- Deciders: project maintainers
- Amended by: [ADR-0025](0025-native-windows-admin-and-complete-uninstall.md), which removes the HTML/browser client while retaining the independent one-time Admin API session boundary

## Context

[`docs/product.md`](../product.md) calls for a minimal local Admin UI so routine operations do not require remembering commands, and [`docs/architecture.md`](../architecture.md) lists what it must support. [`docs/security.md`](../security.md) recorded the hard part as an open question: "The Admin UI uses a separate local session. The precise bootstrap mechanism for this session remains an open question."

The question is harder than it looks, because a browser is not a well-behaved client. A page on any website can send a request to `http://127.0.0.1:4444`, and the browser will attach whatever cookies that origin has. An attacker's domain can also be made to resolve to `127.0.0.1`, at which point the browser treats their page as same-origin with the daemon. So "the listener is loopback" is not, on its own, a security property against a browser.

That matters here more than it would in most tools. LCTK's central guarantee, verified on the wire in Slice 1.3, is that one project's credential does not open another project. An admin surface sees every project and can start, stop, reindex, and revoke. If a web page could reach it, that guarantee would be worth nothing.

## Decision

### The admin surface shares a listener with the project endpoint and nothing else

No admin handler reads a project grant, and no project route reads an admin session. A coding agent holding a project token cannot reach `/admin`, and an administrator's cookie is meaningless on `/projects/{id}/mcp`. The separation is two independent credential systems rather than one system with a capability flag, because a flag is a thing that can be set by mistake.

### A session is established by a code that is spent once

The daemon generates an exchange code at startup and writes it to the owner-only LCTK home. `lctk admin open` reads it and opens `http://127.0.0.1:4444/admin/?code=…`. The page immediately posts the code, receives a session cookie, and the code is replaced.

The code travels in a URL because that is the only channel a command-line tool has to a browser, and a URL is not a secret store: it survives in history, in a shell scrollback, and in a screenshot. Spending it on first use is what makes that acceptable — what survives is a code that no longer opens anything. The page then clears it from the address bar, so it does not reach history in the first place.

A new code is generated on every daemon start, and the document is removed when the daemon stops. A code left behind by a previous run would sign someone in to this one.

### Three independent defences, all required

- The session cookie is `HttpOnly` and `SameSite=Strict`, so a cross-site request does not carry it and nothing running in a page can read it.
- Every request must carry a `Host` header naming loopback. This is what refuses DNS rebinding: the attacker's hostname still appears in `Host` even after it resolves to `127.0.0.1`, and the check happens before any credential is consulted.
- Every state-changing request must echo the session's CSRF token in a header. A cross-origin page cannot read it, because reading it would require a response the same-origin policy will not hand over.

Each covers a case the others do not, which is why all three are present rather than whichever seemed sufficient.

### The UI is one embedded file

The page is a single HTML file compiled into the binary. No build step, no package manager, no CDN. A build step would put a JavaScript toolchain in front of anyone building a Go program, and a remote script would be a third party with the ability to administer LCTK on the user's machine. It is served with `default-src 'none'` so that even an injection into the page cannot reach the network.

Every value from the API is inserted as text rather than as markup. A project name comes from a folder on disk, and a folder can be named anything.

### Grant tokens are never served to it

The surface lists grants and revokes them. It does not display a token: a page that did would leave one in a screenshot and a browser cache, and the surface exists to manage credentials rather than to distribute them.

## Alternatives considered

- **HTTP Basic authentication.** No CSRF story, no way to revoke a session, and the browser caches the credential for the origin.
- **A long-lived bearer token pasted into the page.** Shifts the same problem to the user and gives them a secret to keep, which is exactly what [ADR-0014](0014-project-credential-delivery.md) avoided for project grants.
- **A separate listener on its own port.** No additional protection: a browser reaches any loopback port. It would only add a second port to configure.
- **A desktop application instead of a web page.** Avoids the browser entirely and its whole threat model, at the cost of a UI toolchain, per-platform packaging, and signing. Not warranted for a surface this small.
- **`Origin` header checking instead of `Host`.** Necessary but not sufficient: `Origin` is absent on same-origin GET requests in some browsers, and DNS rebinding produces a same-origin `Origin` anyway. `Host` is what carries the attacker's name.

## Consequences

### Positive

- A coding session cannot administer the machine, and an administrator's browser session cannot act as a project client.
- Signing in is one command and one click, with no secret for the user to keep.
- The page that ships is the page that runs, with no supply chain between them.

### Negative

- Signing in again after a daemon restart requires running `lctk admin open` again, because the code is regenerated. That is the intended trade.
- The exchange code is briefly in a URL. Spending it on first use bounds the exposure but does not remove it.
- A single embedded page limits how elaborate the UI can become before the decision has to be revisited.

### Follow-up

- The surface has no way to *issue* a grant to a new client yet, only to list and revoke. Issuing is a credential-delivery problem, and [ADR-0014](0014-project-credential-delivery.md) covers the case that exists today.
- Session lifetime is 12 hours by value, not by measurement of how anyone actually uses it.
- The page polls on demand rather than streaming. Index progress is therefore what it was when the page last loaded, which is honest but not live.
