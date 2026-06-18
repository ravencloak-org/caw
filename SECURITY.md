# Security Policy

## Supported Versions

| Version | Supported |
| --- | --- |
| v0.1.x | Latest only |

## Reporting a Vulnerability

Email `security@ravencloak.org` with reproduction steps, affected versions, and any
proof-of-concept material. Please do not file a public GitHub issue for suspected
vulnerabilities.

If that mailbox is not provisioned, open a GitHub Security Advisory via the [Security tab](https://github.com/ravencloak-org/caw/security/advisories/new).

## Expected response

Within 5 business days.

## Scope

- Hub HTTP surface
- Watcher MCP surface
- GitHub App credentials handling
- Webhook signature verification

## Out of scope

- Customer-controlled `.env` contents
- Third-party harness vulnerabilities
