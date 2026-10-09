<!-- BEGIN MICROSOFT SECURITY.MD V1.0.0 BLOCK -->

## Security

Microsoft takes the security of our software products and services seriously, which
includes all source code repositories in our GitHub organizations.

**Please do not report security vulnerabilities through public GitHub issues.**

For security reporting information, locations, contact information, and policies,
please review the latest guidance for Microsoft repositories at
[https://aka.ms/SECURITY.md](https://aka.ms/SECURITY.md).

<!-- END MICROSOFT SECURITY.MD BLOCK -->

## Release SBOMs

Coordinated TauGrid GitHub Releases include machine-readable SPDX JSON SBOMs
for every published `tau` and `tau-gen` CLI binary and for the `tau`,
`taugrid-portal`, and `tau-core-controller` images. Use
`taugrid-release-sbom-index.json` and `SHA256SUMS` from the same release to map
versions and immutable image digests to exact SBOM assets and verify their
downloaded bytes.

Image SBOMs are generated from public MCR images resolved to
immutable digests. Separate Linux amd64 and arm64 assets are bound to their
platform manifest digests, while the release index also preserves the
top-level OCI image-index digest. The GitHub release boundary does not publish
OCI referrers, attestations, or signatures. An SBOM is an inventory, not a
claim that a component is vulnerability-free or compliant with a particular
policy.
