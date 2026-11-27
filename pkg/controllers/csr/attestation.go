/*
Copyright 2026 The Karpenter GCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package csr

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"

	"github.com/google/go-tpm/tpm2"
)

// The GKE exec auth plugin appends these blocks to a kubelet client CSR:
// https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/cmd/gke-exec-auth-plugin/request.go
// The proof certifies the CSR public key with the VM attestation key; it does
// not sign a controller nonce, CSR UID, NodeClaim UID or cluster audience.
const (
	csrRequestBlock           = "CERTIFICATE REQUEST"
	vmIdentityBlock           = "VM IDENTITY"
	attestationDataBlock      = "ATTESTATION DATA"
	attestationSignatureBlock = "ATTESTATION SIGNATURE"
	attestationCertBlock      = "ATTESTATION CERTIFICATE"
)

// vmIdentity is the untrusted VM IDENTITY payload of an attestation envelope.
// Every field is compared with authoritative Compute Engine data before use.
type vmIdentity struct {
	Zone string `json:"zone"`
	ID   uint64 `json:"id"`
	Name string `json:"name"`
	// GKE names these confusingly: project_id is the numeric project number and
	// project_name is the string project ID.
	ProjectNumber uint64 `json:"project_id"`
	ProjectID     string `json:"project_name"`
}

// attestationProof is the parsed, not yet verified, TPM attestation envelope of
// a kubelet client CSR.
type attestationProof struct {
	vm                   vmIdentity
	attestationData      []byte
	attestationSignature []byte
}

// parseAttestationEnvelope parses the complete attestation envelope. The
// envelope must contain the four required blocks of a GKE TPM proof, each once,
// with no unsupported block types, headers or trailing data. The producer may
// also append an ATTESTATION CERTIFICATE, which is ignored: the authoritative
// attestation key always comes from Compute, as in the pinned upstream verifier.
// The embedded CSR must be the CSR that was already parsed from the request.
func parseAttestationEnvelope(request []byte, csr *x509.CertificateRequest) (attestationProof, error) {
	blocks, err := parseAttestationBlocks(request)
	if err != nil {
		return attestationProof{}, err
	}
	if !bytes.Equal(blocks[csrRequestBlock], csr.Raw) {
		return attestationProof{}, fmt.Errorf("attestation envelope embeds a different certificate request")
	}
	proof := attestationProof{
		attestationData:      blocks[attestationDataBlock],
		attestationSignature: blocks[attestationSignatureBlock],
	}
	if err := json.Unmarshal(blocks[vmIdentityBlock], &proof.vm); err != nil {
		return attestationProof{}, fmt.Errorf("parsing VM IDENTITY block: %w", err)
	}
	if proof.vm.ProjectNumber == 0 || proof.vm.ID == 0 || proof.vm.ProjectID == "" || proof.vm.Zone == "" || proof.vm.Name == "" {
		return attestationProof{}, fmt.Errorf("VM IDENTITY block is missing required identity fields")
	}
	return proof, nil
}

// pemBegin is the first line prefix of a PEM block.
var pemBegin = []byte("-----BEGIN ")

// parseAttestationBlocks returns the required proof blocks and, when present,
// the native producer's optional attestation certificate.
func parseAttestationBlocks(request []byte) (map[string][]byte, error) {
	blocks := map[string][]byte{}
	raw := bytes.TrimSpace(request)
	for len(raw) > 0 {
		block, rest, err := decodeAttestationBlock(raw)
		if err != nil {
			return nil, err
		}
		if len(block.Headers) > 0 {
			return nil, fmt.Errorf("attestation envelope contains a PEM block with headers")
		}
		switch block.Type {
		case csrRequestBlock, vmIdentityBlock, attestationDataBlock, attestationSignatureBlock, attestationCertBlock:
		default:
			return nil, fmt.Errorf("attestation envelope contains unsupported PEM block %q", block.Type)
		}
		if _, ok := blocks[block.Type]; ok {
			return nil, fmt.Errorf("attestation envelope contains more than one %q block", block.Type)
		}
		blocks[block.Type] = block.Bytes
		raw = bytes.TrimSpace(rest)
	}
	for _, name := range []string{csrRequestBlock, vmIdentityBlock, attestationDataBlock, attestationSignatureBlock} {
		if len(blocks[name]) == 0 {
			return nil, fmt.Errorf("attestation envelope is missing the %q block", name)
		}
	}
	return blocks, nil
}

// decodeAttestationBlock decodes the block that starts raw. pem.Decode skips a
// malformed block and returns a later one, so the consumed prefix must contain
// exactly one BEGIN marker; otherwise malformed data could be silently dropped.
func decodeAttestationBlock(raw []byte) (*pem.Block, []byte, error) {
	if !bytes.HasPrefix(raw, pemBegin) {
		return nil, nil, fmt.Errorf("attestation envelope has trailing data that is not a PEM block")
	}
	block, rest := pem.Decode(raw)
	if block == nil {
		return nil, nil, fmt.Errorf("attestation envelope contains an invalid PEM block")
	}
	if bytes.Count(raw[:len(raw)-len(rest)], pemBegin) != 1 {
		return nil, nil, fmt.Errorf("attestation envelope contains malformed data before a PEM block")
	}
	return block, rest, nil
}

// parseAttestationKey parses the authoritative VM attestation key returned by
// the Compute API. Only RSA keys are accepted because the TPM certification
// signature is verified with PKCS#1 v1.5 SHA-256.
func parseAttestationKey(pemEncoded string) (*rsa.PublicKey, error) {
	block, rest := pem.Decode([]byte(pemEncoded))
	if block == nil || block.Type != "PUBLIC KEY" || len(block.Headers) > 0 {
		return nil, fmt.Errorf("VM attestation key is not a PEM-encoded public key")
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, fmt.Errorf("VM attestation key has trailing data")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing VM attestation key: %w", err)
	}
	rsaKey, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("VM attestation key is %T, expected *rsa.PublicKey", pub)
	}
	return rsaKey, nil
}

// verifyAttestationProof verifies the TPM certification signature with the
// authoritative VM attestation key and binds the certified TPM object to the
// public key of the CSR.
func verifyAttestationProof(proof attestationProof, csr *x509.CertificateRequest, attestationKey *rsa.PublicKey) error {
	hash := sha256.Sum256(proof.attestationData)
	if err := rsa.VerifyPKCS1v15(attestationKey, crypto.SHA256, hash[:], proof.attestationSignature); err != nil {
		return fmt.Errorf("verifying TPM certification signature with the VM attestation key: %w", err)
	}

	// Unmarshal tolerates trailing bytes, so re-encode and compare to reject a
	// non-canonical certification structure.
	attestationData, err := tpm2.Unmarshal[tpm2.TPMSAttest](proof.attestationData)
	if err != nil {
		return fmt.Errorf("parsing TPM certification data: %w", err)
	}
	// go-tpm's unmarshaller does not enforce the TPM_GENERATED magic tag, so check
	// it explicitly: it marks the structure as produced by the TPM itself.
	if err := attestationData.Magic.Check(); err != nil {
		return fmt.Errorf("TPM certification data: %w", err)
	}
	if attestationData.Type != tpm2.TPMSTAttestCertify {
		return fmt.Errorf("TPM attestation data is not a certify structure")
	}
	if !bytes.Equal(tpm2.Marshal(attestationData), proof.attestationData) {
		return fmt.Errorf("TPM certification data is not canonical or has trailing data")
	}
	certifyInfo, err := attestationData.Attested.Certify()
	if err != nil {
		return fmt.Errorf("reading TPM certify info: %w", err)
	}

	public, err := gkeKubeletPublic(csr.PublicKey)
	if err != nil {
		return fmt.Errorf("converting the CSR public key to a TPM public structure: %w", err)
	}
	name, err := tpm2.ObjectName(public)
	if err != nil {
		return fmt.Errorf("computing the TPM name of the CSR public key: %w", err)
	}
	if !bytes.Equal(name.Buffer, certifyInfo.Name.Buffer) {
		return errors.New("TPM-certified public object does not match the CSR public key")
	}
	return nil
}

var gkeTPMCurves = map[string]tpm2.TPMECCCurve{
	elliptic.P224().Params().Name: tpm2.TPMECCNistP224,
	elliptic.P256().Params().Name: tpm2.TPMECCNistP256,
	elliptic.P384().Params().Name: tpm2.TPMECCNistP384,
	elliptic.P521().Params().Name: tpm2.TPMECCNistP521,
}

// gkeKubeletPublic reconstructs GKE's imported kubelet-key TPM template.
//
// Adapted from Google's GKE certificate approval helper:
// https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/pkg/tpmattest/tpmattest.go
// Copyright 2018 The Kubernetes Authors.
// Licensed under the Apache License, Version 2.0.
// Changes: uses go-tpm's tpm2 structures and standard-library public-key
// encoding, preserving the producer's minimally encoded coordinates.
func gkeKubeletPublic(pub crypto.PublicKey) (*tpm2.TPMTPublic, error) {
	ecKey, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key in CSR is %T, only ECDSA keys supported", pub)
	}

	curveName := ecKey.Curve.Params().Name
	curve, ok := gkeTPMCurves[curveName]
	if !ok {
		return nil, fmt.Errorf("public key EC curve %q not supported", curveName)
	}
	point, err := ecKey.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encoding public key point: %w", err)
	}
	// Bytes returns a validated SEC1 point. GKE's template uses minimal integer
	// coordinates, not SEC1's fixed-width coordinates; padding changes the Name.
	size := (len(point) - 1) / 2
	return &tpm2.TPMTPublic{
		Type:    tpm2.TPMAlgECC,
		NameAlg: tpm2.TPMAlgSHA256,
		// Allow access for TPM user with password auth. Sign is required to load
		// the key (either Sign or Decrypt).
		ObjectAttributes: tpm2.TPMAObject{SignEncrypt: true, UserWithAuth: true},
		Parameters: tpm2.NewTPMUPublicParms(tpm2.TPMAlgECC, &tpm2.TPMSECCParms{
			Symmetric: tpm2.TPMTSymDefObject{Algorithm: tpm2.TPMAlgNull},
			Scheme: tpm2.TPMTECCScheme{
				Scheme: tpm2.TPMAlgECDSA,
				Details: tpm2.NewTPMUAsymScheme(tpm2.TPMAlgECDSA, &tpm2.TPMSSigSchemeECDSA{
					HashAlg: tpm2.TPMAlgSHA256,
				}),
			},
			CurveID: curve,
			KDF:     tpm2.TPMTKDFScheme{Scheme: tpm2.TPMAlgNull},
		}),
		Unique: tpm2.NewTPMUPublicID(tpm2.TPMAlgECC, &tpm2.TPMSECCPoint{
			X: tpm2.TPM2BECCParameter{Buffer: bytes.TrimLeft(point[1:1+size], "\x00")},
			Y: tpm2.TPM2BECCParameter{Buffer: bytes.TrimLeft(point[1+size:], "\x00")},
		}),
	}, nil
}
