# Security Policy

LHA executes model-generated code and tools, so security is a first-class concern. The
architecture assumes the agent **will** be prompt-injected over a long run and contains the blast
radius in code (not prompts):

- **Default-deny egress** + an allow-list, with **credential brokering** (the agent only ever
  holds placeholder tokens; real secrets are injected at the egress boundary).
- **Sandboxed execution** (local → Docker → E2B/Firecracker) so untrusted code can't reach the host.
- **Tool allow-list + argument validation** enforced in the dispatcher.
- **Rule of Two** — no single session simultaneously holds untrusted content, private data, and
  external comms.
- **Deterministic verification** as the only merge gate (never the model's say-so).

## Reporting a vulnerability

Please **do not open a public issue** for security vulnerabilities. Instead, email the maintainer,
Calvin Cheng, at [calvin@calvinx.com](mailto:calvin@calvinx.com) with:

- a description and impact,
- reproduction steps or a proof of concept,
- affected version/commit.

You'll get an acknowledgment within a few days. Please allow reasonable time for a fix before any
public disclosure.

## Scope notes

- This is research-grade software under active development. Run untrusted missions only inside a
  real sandbox (Docker/E2B), never the `local` sandbox.
- Keep API keys in `.env` (git-ignored) or a secrets manager — never commit them.
