# TauGrid Governance

TauGrid is a Microsoft-led open-source project maintained in the Azure GitHub
organization. This document describes project roles and decision making. It does
not replace the Microsoft Open Source Code of Conduct, security policy, or
Microsoft's legal and release requirements.

## Principles

- Project work and technical decisions should be visible in GitHub issues and
  pull requests whenever they do not contain confidential or security-sensitive
  information.
- Maintainers seek practical consensus and explain decisions that materially
  affect users, contributors, compatibility, or project direction.
- Security, privacy, licensing, trademark, and Microsoft release-policy
  requirements are mandatory constraints.
- Public product source has one owner in this repository. Private deployment
  repositories consume released artifacts rather than maintaining a second copy.

## Roles

### Contributors

Contributors report issues, propose designs, improve documentation and examples,
write code and tests, and participate in review. Anyone following the Code of
Conduct and contribution requirements may contribute.

### Reviewers

Reviewers are trusted contributors who regularly review an area of the project.
They provide technical feedback and may recommend that a change be accepted, but
formal approval and merge authority remain with the maintainers. Reviewers do not
receive repository administration or release authority solely through this role.

### Maintainers

Maintainers are members of the `@Azure/taugrid-maintainers` GitHub team or successor
team designated in repository policy. They are responsible for:

- project direction and public API stewardship;
- issue triage and pull-request review;
- code ownership and merge decisions;
- release quality, artifact promotion, and compatibility policy;
- security, dependency, and vulnerability response;
- contributor experience and Code of Conduct enforcement; and
- keeping governance and ownership records current.

Repository administrators may perform organization-level configuration, access,
and incident-response tasks on behalf of maintainers.

## Decision Making

Routine changes are decided through review and approval by members of
`@Azure/taugrid-maintainers`. The approving maintainer is accountable for
correctness, compatibility, tests, documentation, and the public release
boundary.

Changes with broad or difficult-to-reverse impact should begin with a GitHub
issue or design proposal. Examples include:

- public APIs, CRDs, configuration schemas, or CLI compatibility;
- repository structure and component ownership;
- new required services or major dependencies;
- security or identity models;
- release, support, or deprecation policy; and
- governance or maintainer changes.

Maintainers seek consensus within `@Azure/taugrid-maintainers`, consulting
affected reviewers and contributors as appropriate. If consensus cannot be
reached, the change does not proceed until the disagreement is resolved; the
maintainers should record the alternatives and unresolved concerns.

Microsoft legal, security, privacy, trademark, or service requirements may
override a technical preference. Confidential details remain in the appropriate
internal system, while the public outcome is documented when possible.

## Reviews and Merges

- Required CI must pass.
- At least one appropriate code owner must approve.
- Authors do not approve their own pull requests.
- Security-sensitive, cross-component, governance, or public-contract changes may
  require additional owners.
- Maintainers may close changes that are unsafe, out of scope, abandoned, or
  incompatible with the project's direction, with an explanation when possible.

Maintainers may use squash, rebase, or merge commits according to repository
policy. Commit and pull-request text must remain understandable in project
history.

## Releases and Security

Only authorized maintainers and protected automation publish releases. Releases
must follow the project's versioning and promotion policy and pass required
build, test, provenance, SBOM, vulnerability, and license checks.

Security reports follow [SECURITY.md](SECURITY.md), not public issue triage.
Maintainers coordinate embargoed fixes through approved private channels and
publish advisories when appropriate.

## Adding or Removing Maintainers

Maintainer candidates should demonstrate sustained, constructive contribution,
sound technical judgment, reliable review, respect for compatibility and
security, and adherence to the Code of Conduct.

Existing maintainers nominate candidates. Adding a maintainer requires consensus
of the active members of `@Azure/taugrid-maintainers` and completion of Azure
organization access requirements. The team records the resulting ownership
change in repository policy and `CODEOWNERS`.

A maintainer may step down at any time. Maintainer access may also be removed for
extended inactivity, role change, security needs, or Code of Conduct violations.
Removal decisions are made by consensus of the remaining active maintainers and,
when repository or organization access is affected, the relevant Microsoft
organization owners. Access is removed promptly when required for security.

## Governance Changes

Changes to this document use the same public pull-request process and require
approval from the owning maintainer team. Material changes should include an
issue describing the reason and expected effect.
