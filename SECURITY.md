# Security Policy

Crypto Exchange handles authentication, personal data, and eventually customer funds, so security reports are taken seriously and are always welcome.

## Project status

The project is **pre-alpha** and is not intended for production use. Findings from internal reviews are tracked publicly with the [`type: security`](https://github.com/mdastpak/Crypto-Exchange/issues?q=is%3Aissue+label%3A%22type%3A+security%22) label. Before reporting, check whether your finding is already listed there.

## Supported versions

No releases have been published yet. Security fixes are applied to the `main` branch only.

| Version | Supported |
| --- | --- |
| `main` | Yes |
| Any other branch or fork | No |

## Reporting a vulnerability

**Do not open a public issue, pull request, or discussion for a vulnerability that is not already publicly tracked.**

Report it privately through GitHub:

1. Open the [**Report a vulnerability**](https://github.com/mdastpak/Crypto-Exchange/security/advisories/new) form. You can also reach it from the repository's **Security** tab, under **Advisories**.
2. Include as much of the following as possible:
   - The affected component, file, or endpoint, and the commit hash you tested
   - The type of issue (for example authentication bypass, injection, or information disclosure)
   - Step-by-step reproduction instructions or a proof of concept
   - The impact you expect, and any preconditions
   - Suggested remediation, if you have one

## What to expect

These are goals for a volunteer-maintained project, not contractual commitments:

| Stage | Target |
| --- | --- |
| Acknowledgement of your report | Within 3 business days |
| Initial assessment and severity rating | Within 10 business days |
| Fix or mitigation plan for confirmed critical and high issues | Within 30 days |
| Status updates | At least every 14 days until the report is resolved |

Confirmed vulnerabilities are fixed in a private fork through the GitHub advisory workflow. A GitHub Security Advisory, with a CVE where applicable, is published once a fix is available. Reporters are credited in the advisory unless they ask not to be.

## Coordinated disclosure

Please give us a reasonable amount of time to fix an issue before you disclose it publicly. We aim to agree on a disclosure date with you, normally within 90 days of the report.

## Scope

In scope:

- Source code in this repository, including the HTTP and WebSocket API, authentication and session handling, database migrations, configuration defaults, and the Docker setup
- Cryptographic and key-management weaknesses in the code base

Out of scope:

- Vulnerabilities in third-party dependencies that do not affect this project's use of them. Report those upstream. Tell us anyway if this project is affected.
- Findings that require physical access, a compromised host, or social engineering
- Denial of service through volumetric traffic alone
- Reports generated only by automated scanners, without a demonstrated impact

## Safe harbor

We will not pursue legal action against anyone who researches and reports vulnerabilities in good faith under this policy. Good faith means that you avoid privacy violations, data destruction, and service disruption, that you only test against your own local deployment, and that you give us reasonable time to respond before disclosure.

## Security practices for contributors

- Never commit secrets, private keys, or real personal data. Use environment variables or local, Git-ignored configuration files. See [docs/configuration.md](docs/configuration.md#secrets).
- Never log one-time codes, tokens, passwords, or unmasked contact details.
- Use `crypto/rand` for anything security-sensitive, and constant-time comparison for secrets.
- Use parameterized SQL only.
- Use `shared.Decimal` for monetary values. Never use floating point.
- Add tests for every security fix, and reference the related issue in the pull request.

Dependency vulnerabilities are monitored through Dependabot alerts.
