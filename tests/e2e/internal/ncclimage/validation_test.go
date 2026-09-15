// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ncclimage

import "testing"

func TestValidateAcceptsOnlyQualifiedNVIDIALinuxAMD64Child(t *testing.T) {
	if err := Validate(Image, Repository, IndexDigest, LinuxAMD64Digest, LinuxAMD64Config); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsEveryCoordinateDeviation(t *testing.T) {
	tests := map[string]struct {
		image, repository, indexDigest, childDigest, configDigest string
	}{
		"floating tag": {
			image: Repository + ":" + QualifiedTag, repository: Repository,
			indexDigest: IndexDigest, childDigest: LinuxAMD64Digest, configDigest: LinuxAMD64Config,
		},
		"index as runtime": {
			image: Repository + "@" + IndexDigest, repository: Repository,
			indexDigest: IndexDigest, childDigest: IndexDigest, configDigest: LinuxAMD64Config,
		},
		"other nvidia repository": {
			image: "nvcr.io/nvidia/nccl@" + LinuxAMD64Digest, repository: "nvcr.io/nvidia/nccl",
			indexDigest: IndexDigest, childDigest: LinuxAMD64Digest, configDigest: LinuxAMD64Config,
		},
		"other registry": {
			image: "ghcr.io/example/pytorch@" + LinuxAMD64Digest, repository: "ghcr.io/example/pytorch",
			indexDigest: IndexDigest, childDigest: LinuxAMD64Digest, configDigest: LinuxAMD64Config,
		},
		"wrong index": {
			image: Image, repository: Repository,
			indexDigest: LinuxAMD64Config, childDigest: LinuxAMD64Digest, configDigest: LinuxAMD64Config,
		},
		"wrong child": {
			image: Image, repository: Repository,
			indexDigest: IndexDigest, childDigest: LinuxAMD64Config, configDigest: LinuxAMD64Config,
		},
		"wrong config": {
			image: Image, repository: Repository,
			indexDigest: IndexDigest, childDigest: LinuxAMD64Digest, configDigest: IndexDigest,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if err := Validate(test.image, test.repository, test.indexDigest, test.childDigest, test.configDigest); err == nil {
				t.Fatal("Validate() succeeded, want error")
			}
		})
	}
}

func TestQualifiedSupplyChainEvidence(t *testing.T) {
	evidence := QualifiedSupplyChainEvidence()
	if err := ValidateSupplyChainEvidence(evidence); err != nil {
		t.Fatalf("ValidateSupplyChainEvidence() error = %v", err)
	}
	if evidence.SBOMManifestDigest != "sha256:0af00f2f7dab7b4902a22efb216ee00712cafc6b0cbddcfdc445b9119e47e98c" {
		t.Fatalf("SBOM manifest digest = %q", evidence.SBOMManifestDigest)
	}
	if evidence.SBOMLayerDigest != "sha256:90cf0f3cec44196742b8ed1029608f88d82e524c3dbd863f949b5082572019ab" {
		t.Fatalf("SBOM layer digest = %q", evidence.SBOMLayerDigest)
	}
	if evidence.VEXManifestDigest != "sha256:e145aacd138af26a19ed98be4b548734594d690e99892f9e046f89aa813e09b9" {
		t.Fatalf("VEX manifest digest = %q", evidence.VEXManifestDigest)
	}
	if evidence.VEXLayerDigest != "sha256:72706fd66e18d27f4cc94d09b7501e6d1ce7dc2421f68e5402fd36e9e507390f" {
		t.Fatalf("VEX layer digest = %q", evidence.VEXLayerDigest)
	}
	if evidence.SignatureManifestDigest != "sha256:6ad472c869b3a886a0c53aba08dcef23233bab714baa5139ba7d9526a135f231" {
		t.Fatalf("signature manifest digest = %q", evidence.SignatureManifestDigest)
	}
	if evidence.SignatureLayerDigest != "sha256:ce5ef2dd676dea4c8e4e74af4473a803f35bd491f68788441da8e795b0512f2b" {
		t.Fatalf("signature layer digest = %q", evidence.SignatureLayerDigest)
	}
	if evidence.VEXFindings != 379 || evidence.VEXExploitable != 262 ||
		evidence.VEXInTriage != 110 || evidence.VEXNotAffected != 7 {
		t.Fatalf("VEX counts = total:%d exploitable:%d in-triage:%d not-affected:%d",
			evidence.VEXFindings, evidence.VEXExploitable, evidence.VEXInTriage, evidence.VEXNotAffected)
	}
	if evidence.VEXHasSeverityRatings {
		t.Fatal("VEX unexpectedly records severity ratings")
	}
	if !evidence.SignatureDigestBound || evidence.SignatureTrustVerified {
		t.Fatal("signature trust caveat changed")
	}
	if evidence.LicensePath != "/workspace/license.txt" || evidence.HasOCILicenseLabel {
		t.Fatal("license evidence changed")
	}
}

func TestValidateSupplyChainEvidenceFailsClosed(t *testing.T) {
	tests := map[string]func(*SupplyChainEvidence){
		"malformed digest": func(evidence *SupplyChainEvidence) {
			evidence.SBOMManifestDigest = "sha256:ABC"
		},
		"duplicate digest": func(evidence *SupplyChainEvidence) {
			evidence.VEXLayerDigest = evidence.SBOMLayerDigest
		},
		"different valid digest": func(evidence *SupplyChainEvidence) {
			evidence.SBOMLayerDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		},
		"VEX count mismatch": func(evidence *SupplyChainEvidence) {
			evidence.VEXFindings++
		},
		"different VEX disposition": func(evidence *SupplyChainEvidence) {
			evidence.VEXExploitable--
			evidence.VEXInTriage++
		},
		"VEX severity claim": func(evidence *SupplyChainEvidence) {
			evidence.VEXHasSeverityRatings = true
		},
		"unverified signature promoted": func(evidence *SupplyChainEvidence) {
			evidence.SignatureTrustVerified = true
		},
		"license path": func(evidence *SupplyChainEvidence) {
			evidence.LicensePath = "/licenses/unknown"
		},
		"OCI license label": func(evidence *SupplyChainEvidence) {
			evidence.HasOCILicenseLabel = true
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			evidence := QualifiedSupplyChainEvidence()
			mutate(&evidence)
			if err := ValidateSupplyChainEvidence(evidence); err == nil {
				t.Fatal("ValidateSupplyChainEvidence() succeeded, want error")
			}
		})
	}
}
