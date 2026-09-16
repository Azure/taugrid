// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package ncclimage validates the one NVIDIA PyTorch image qualified for the
// NCCL/RDMA Indexed Job diagnostic.
package ncclimage

import (
	"fmt"
	"regexp"
)

const (
	Repository       = "nvcr.io/nvidia/pytorch"
	QualifiedTag     = "25.11-py3"
	IndexDigest      = "sha256:417cbf33f87b5378849df37983552cd1f8bc8b62fe1ceabe004de816a55dff21"
	LinuxAMD64Digest = "sha256:e14cf0da7ca0d878d0874eb81062b77df275491d4a8d030a2a7463a4e8b07f01"
	LinuxAMD64Config = "sha256:06faed719d1bbd31d7053c96d0c94fce4f2b5f92fd95f07d60ec323c9744f118"
	Image            = Repository + "@" + LinuxAMD64Digest
	QualifiedCUDA    = "13.0.2"
	QualifiedNCCL    = "2.28.8"
	QualifiedPyTorch = "2.10.0a0+b558c98"

	SBOMManifestDigest      = "sha256:0af00f2f7dab7b4902a22efb216ee00712cafc6b0cbddcfdc445b9119e47e98c"
	SBOMLayerDigest         = "sha256:90cf0f3cec44196742b8ed1029608f88d82e524c3dbd863f949b5082572019ab"
	VEXManifestDigest       = "sha256:e145aacd138af26a19ed98be4b548734594d690e99892f9e046f89aa813e09b9"
	VEXLayerDigest          = "sha256:72706fd66e18d27f4cc94d09b7501e6d1ce7dc2421f68e5402fd36e9e507390f"
	SignatureManifestDigest = "sha256:6ad472c869b3a886a0c53aba08dcef23233bab714baa5139ba7d9526a135f231"
	SignatureLayerDigest    = "sha256:ce5ef2dd676dea4c8e4e74af4473a803f35bd491f68788441da8e795b0512f2b"
)

type SupplyChainEvidence struct {
	SBOMManifestDigest      string
	SBOMLayerDigest         string
	VEXManifestDigest       string
	VEXLayerDigest          string
	SignatureManifestDigest string
	SignatureLayerDigest    string
	VEXFindings             int
	VEXExploitable          int
	VEXInTriage             int
	VEXNotAffected          int
	VEXHasSeverityRatings   bool
	SignatureDigestBound    bool
	SignatureTrustVerified  bool
	LicensePath             string
	LicenseTerms            string
	HasOCILicenseLabel      bool
}

func QualifiedSupplyChainEvidence() SupplyChainEvidence {
	return SupplyChainEvidence{
		SBOMManifestDigest:      SBOMManifestDigest,
		SBOMLayerDigest:         SBOMLayerDigest,
		VEXManifestDigest:       VEXManifestDigest,
		VEXLayerDigest:          VEXLayerDigest,
		SignatureManifestDigest: SignatureManifestDigest,
		SignatureLayerDigest:    SignatureLayerDigest,
		VEXFindings:             379,
		VEXExploitable:          262,
		VEXInTriage:             110,
		VEXNotAffected:          7,
		VEXHasSeverityRatings:   false,
		SignatureDigestBound:    true,
		SignatureTrustVerified:  false,
		LicensePath:             "/workspace/license.txt",
		LicenseTerms:            "NVIDIA Software License Agreement plus NVIDIA AI Product Agreement terms",
		HasOCILicenseLabel:      false,
	}
}

var sha256DigestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func Validate(image, repository, indexDigest, linuxAMD64Digest, configDigest string) error {
	if err := ValidateSupplyChainEvidence(QualifiedSupplyChainEvidence()); err != nil {
		return fmt.Errorf("qualified supply-chain evidence: %w", err)
	}
	if repository != Repository {
		return fmt.Errorf("repository must exactly equal %s", Repository)
	}
	if indexDigest != IndexDigest {
		return fmt.Errorf("image-index digest does not match the qualified %s index", QualifiedTag)
	}
	if linuxAMD64Digest != LinuxAMD64Digest {
		return fmt.Errorf("linux/amd64 child digest does not match the qualified child manifest")
	}
	if configDigest != LinuxAMD64Config {
		return fmt.Errorf("linux/amd64 config digest does not match the qualified image config")
	}
	if indexDigest == linuxAMD64Digest {
		return fmt.Errorf("linux/amd64 child digest must differ from the image-index digest")
	}
	if image != Image {
		return fmt.Errorf("runtime image must exactly equal %s", Image)
	}
	return nil
}

func ValidateSupplyChainEvidence(evidence SupplyChainEvidence) error {
	digests := map[string]string{
		"image index":        IndexDigest,
		"linux/amd64 child":  LinuxAMD64Digest,
		"image config":       LinuxAMD64Config,
		"SBOM manifest":      evidence.SBOMManifestDigest,
		"SBOM layer":         evidence.SBOMLayerDigest,
		"VEX manifest":       evidence.VEXManifestDigest,
		"VEX layer":          evidence.VEXLayerDigest,
		"signature manifest": evidence.SignatureManifestDigest,
		"signature layer":    evidence.SignatureLayerDigest,
	}
	expectedEvidenceDigests := map[string]string{
		"SBOM manifest":      SBOMManifestDigest,
		"SBOM layer":         SBOMLayerDigest,
		"VEX manifest":       VEXManifestDigest,
		"VEX layer":          VEXLayerDigest,
		"signature manifest": SignatureManifestDigest,
		"signature layer":    SignatureLayerDigest,
	}
	seen := make(map[string]string, len(digests))
	for name, digest := range digests {
		if !sha256DigestRE.MatchString(digest) {
			return fmt.Errorf("%s digest must be sha256 followed by 64 lowercase hexadecimal characters", name)
		}
		if expected, qualified := expectedEvidenceDigests[name]; qualified && digest != expected {
			return fmt.Errorf("%s digest does not match the qualified evidence", name)
		}
		if previous, duplicate := seen[digest]; duplicate {
			return fmt.Errorf("%s digest duplicates %s digest", name, previous)
		}
		seen[digest] = name
	}
	if evidence.VEXFindings != evidence.VEXExploitable+evidence.VEXInTriage+evidence.VEXNotAffected {
		return fmt.Errorf("VEX status counts do not sum to the finding count")
	}
	if evidence.VEXFindings != 379 || evidence.VEXExploitable != 262 ||
		evidence.VEXInTriage != 110 || evidence.VEXNotAffected != 7 {
		return fmt.Errorf("VEX finding status counts do not match the qualified evidence")
	}
	if evidence.VEXHasSeverityRatings {
		return fmt.Errorf("qualified VEX evidence must record that severity ratings are absent")
	}
	if !evidence.SignatureDigestBound || evidence.SignatureTrustVerified {
		return fmt.Errorf("signature evidence must remain digest-bound and not trust-verified from registry metadata alone")
	}
	if evidence.LicensePath != "/workspace/license.txt" {
		return fmt.Errorf("qualified license path changed")
	}
	if evidence.LicenseTerms != "NVIDIA Software License Agreement plus NVIDIA AI Product Agreement terms" {
		return fmt.Errorf("qualified license terms changed")
	}
	if evidence.HasOCILicenseLabel {
		return fmt.Errorf("qualified image must record that no OCI license label was observed")
	}
	return nil
}
