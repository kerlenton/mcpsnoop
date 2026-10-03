# Security Policy

## Reporting a vulnerability

Please report security issues privately rather than as a public issue, so a fix
can land before the details are out. Use GitHub's private reporting on the
[Security Advisories](https://github.com/kerlenton/mcpsnoop/security/advisories/new)
page. You can expect a response within a few days.

mcpsnoop runs the server command you wrap and captures MCP payloads that may
include credentials, so treat saved traces as sensitive and see the redaction
options in [docs/REDACTION.md](docs/REDACTION.md) before you share one.

mcpsnoop also reads `.mcpsnoop.toml` from the current working directory. Only
use it in directories you trust, since a config file can change tracing
behavior (for example, `no-trace`) or override the trace output path.

## Supported versions

Security fixes land only on the latest release, so please update to the newest
version and check the issue is still there before reporting it.
