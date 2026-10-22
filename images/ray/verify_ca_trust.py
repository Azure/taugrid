# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import hashlib
import os
import ssl
import subprocess
from importlib.metadata import version
from pathlib import Path

import certifi


def bundle_fingerprints(path: Path) -> set[str]:
    fingerprints = set()
    for block in path.read_text().split("-----END CERTIFICATE-----"):
        if "-----BEGIN CERTIFICATE-----" not in block:
            continue
        pem = block[block.index("-----BEGIN CERTIFICATE-----") :]
        der = ssl.PEM_cert_to_DER_cert(pem + "-----END CERTIFICATE-----\n")
        fingerprints.add(hashlib.sha256(der).hexdigest().upper())
    return fingerprints


def numeric_version(value: str) -> tuple[int, ...]:
    return tuple(int(part) for part in value.split("."))


system_bundle = Path("/etc/pki/tls/certs/ca-bundle.crt")

for name in ("SSL_CERT_FILE", "CURL_CA_BUNDLE", "PIP_CERT"):
    if os.environ.get(name) != str(system_bundle):
        raise RuntimeError(f"{name} does not resolve to {system_bundle}")

ssl.create_default_context(cafile=str(system_bundle))

actual_ca_version = subprocess.run(
    ["rpm", "-q", "--qf", "%{VERSION}-%{RELEASE}", "ca-certificates"],
    check=True,
    capture_output=True,
    text=True,
).stdout

actual_certifi_version = version("certifi")
if numeric_version(actual_certifi_version) < numeric_version("2026.10.19"):
    raise RuntimeError(
        f"certifi {actual_certifi_version} is older than 2026.10.19"
    )

system_distrusted = {
    "AAA Certificate Services": (
        "D7A7A0FB5D7E2731D771E9484EBCDEF71D5F0C3E0A2948782BC83EE0EA699EF4"
    ),
    "emSign ECC Root CA - G3": (
        "86A1ECBA089C4A8D3BBE2734C612BA341D813E043CF9E8A862CD5C57A36BBE6B"
    ),
    "emSign Root TLS CA - G3": (
        "7DD78D5F4F13459A83DFF9ABBBA62EDBAF6F2D102BF257FD712F4D9F2746ED8D"
    ),
}
python_distrusted = {
    "emSign ECC Root CA - G3": (
        "86A1ECBA089C4A8D3BBE2734C612BA341D813E043CF9E8A862CD5C57A36BBE6B"
    ),
}

system_fingerprints = bundle_fingerprints(system_bundle)
present_system_distrusted = {
    name: fingerprint
    for name, fingerprint in system_distrusted.items()
    if fingerprint in system_fingerprints
}
if present_system_distrusted:
    raise RuntimeError(
        f"system bundle trusts affected roots: {sorted(present_system_distrusted)}"
    )

python_fingerprints = bundle_fingerprints(Path(certifi.where()))
present_python_distrusted = {
    name: fingerprint
    for name, fingerprint in python_distrusted.items()
    if fingerprint in python_fingerprints
}
if present_python_distrusted:
    raise RuntimeError(
        f"certifi bundle trusts affected roots: {sorted(present_python_distrusted)}"
    )

print(
    f"ca-certificates={actual_ca_version} certifi={actual_certifi_version} "
    f"system_bundle={system_bundle} certifi_bundle={certifi.where()}"
)
