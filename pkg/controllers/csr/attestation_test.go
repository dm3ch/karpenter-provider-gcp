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
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/google/go-tpm/tpm2"
	"github.com/stretchr/testify/require"
)

// attestationVM is the VM identity a valid local attestation fixture claims.
var attestationVM = vmIdentity{
	Zone:          "us-central1-a",
	ID:            987654321,
	Name:          "karpenter-default-abc",
	ProjectNumber: 123456789,
	ProjectID:     "my-project",
}

// pemBlock is one block of a CSR attestation envelope.
type pemBlock struct {
	Type  string
	Bytes []byte
}

func encodePEMBlocks(blocks []pemBlock) []byte {
	var encoded []byte
	for _, block := range blocks {
		encoded = append(encoded, pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: block.Bytes})...)
	}
	return encoded
}

// attestationFixture is a locally generated TPM attestation proof. It contains
// no real cluster, key or certificate material.
type attestationFixture struct {
	vm  vmIdentity
	key *ecdsa.PrivateKey // CSR key
	ak  *rsa.PrivateKey   // VM attestation key
	der []byte            // certificate request DER
	// data and signature are the TPM certify structure and its RSA signature.
	data []byte
	sig  []byte
}

func newAttestationFixture(t testing.TB) *attestationFixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ak, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	fixture := &attestationFixture{vm: attestationVM, key: key, ak: ak}
	fixture.der = certificateRequestDER(t, key, fixture.vm.Name)
	fixture.data, fixture.sig = certifyKey(t, key, ak)
	return fixture
}

func certificateRequestDER(t testing.TB, key *ecdsa.PrivateKey, nodeName string) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   "system:node:" + nodeName,
			Organization: []string{"system:nodes", nodePoolOrg, instanceOrg},
		},
	}, key)
	require.NoError(t, err)
	return der
}

// certifyKey builds a TPM certify structure over the key's TPM public object and
// signs it with the attestation key, as the GKE exec auth plugin does.
func certifyKey(t testing.TB, key *ecdsa.PrivateKey, ak *rsa.PrivateKey) (data, sig []byte) {
	t.Helper()
	public, err := gkeKubeletPublic(key.Public())
	require.NoError(t, err)
	name, err := tpm2.ObjectName(public)
	require.NoError(t, err)
	// A real TPM's qualified name differs from the name; any distinct value works here.
	qualified := tpm2.TPM2BName{Buffer: append([]byte{0}, name.Buffer...)}
	data = tpm2.Marshal(&tpm2.TPMSAttest{
		Magic: tpm2.TPMGeneratedValue,
		Type:  tpm2.TPMSTAttestCertify,
		Attested: tpm2.NewTPMUAttest(tpm2.TPMSTAttestCertify, &tpm2.TPMSCertifyInfo{
			Name:          *name,
			QualifiedName: qualified,
		}),
	})
	return data, signAttestation(t, ak, data)
}

func signAttestation(t testing.TB, ak *rsa.PrivateKey, data []byte) []byte {
	t.Helper()
	hash := sha256.Sum256(data)
	sig, err := rsa.SignPKCS1v15(rand.Reader, ak, crypto.SHA256, hash[:])
	require.NoError(t, err)
	return sig
}

func (f *attestationFixture) vmIdentityBytes(t testing.TB) []byte {
	t.Helper()
	encoded, err := json.Marshal(f.vm)
	require.NoError(t, err)
	return encoded
}

// blocks returns a valid four-block attestation envelope.
func (f *attestationFixture) blocks(t testing.TB) []pemBlock {
	t.Helper()
	return []pemBlock{
		{Type: csrRequestBlock, Bytes: f.der},
		{Type: vmIdentityBlock, Bytes: f.vmIdentityBytes(t)},
		{Type: attestationDataBlock, Bytes: f.data},
		{Type: attestationSignatureBlock, Bytes: f.sig},
	}
}

// attestationKeyPEM returns the authoritative attestation key in the form the
// Compute API returns it.
func (f *attestationFixture) attestationKeyPEM(t testing.TB) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&f.ak.PublicKey)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func TestParseAttestationEnvelope(t *testing.T) {
	fixture := newAttestationFixture(t)
	identityBlock := func(t *testing.T, payload []byte) []byte {
		blocks := fixture.blocks(t)
		blocks[1] = pemBlock{Type: vmIdentityBlock, Bytes: payload}
		return encodePEMBlocks(blocks)
	}
	identityWith := func(mutate func(*vmIdentity)) func(t *testing.T) []byte {
		return func(t *testing.T) []byte {
			identity := fixture.vm
			mutate(&identity)
			encoded, err := json.Marshal(identity)
			require.NoError(t, err)
			return identityBlock(t, encoded)
		}
	}
	missing := func(index int) func(t *testing.T) []byte {
		return func(t *testing.T) []byte {
			blocks := fixture.blocks(t)
			return encodePEMBlocks(append(blocks[:index:index], blocks[index+1:]...))
		}
	}
	empty := func(index int) func(t *testing.T) []byte {
		return func(t *testing.T) []byte {
			blocks := fixture.blocks(t)
			blocks[index].Bytes = nil
			return encodePEMBlocks(blocks)
		}
	}

	for _, tt := range []struct {
		name    string
		request func(t *testing.T) []byte
		valid   bool
	}{
		// Valid shapes.
		{name: "valid envelope", valid: true, request: func(t *testing.T) []byte { return encodePEMBlocks(fixture.blocks(t)) }},
		{name: "valid envelope in a different block order", valid: true, request: func(t *testing.T) []byte {
			blocks := fixture.blocks(t)
			return encodePEMBlocks([]pemBlock{blocks[1], blocks[3], blocks[2], blocks[0]})
		}},
		{
			// pem.Decode skips text before a block. The approver and the Kubernetes
			// signer both read the first decodable block, so the CSR is the same.
			name:  "text skipped by pem.Decode before the envelope",
			valid: true,
			request: func(t *testing.T) []byte {
				return append([]byte("junk\n-----BEGIN CERTIFICATE REQUEST-----\n!!!!not-base64!!!!\n-----END CERTIFICATE REQUEST-----\n"), encodePEMBlocks(fixture.blocks(t))...)
			},
		},

		// Empty, malformed and truncated envelopes.
		{name: "no request", request: func(*testing.T) []byte { return nil }},
		{name: "empty request", request: func(*testing.T) []byte { return []byte{} }},
		{name: "whitespace only", request: func(*testing.T) []byte { return []byte(" \n\t\r\n") }},
		{name: "not PEM", request: func(*testing.T) []byte { return []byte("not a certificate request") }},
		{name: "only the certificate request", request: func(t *testing.T) []byte { return encodePEMBlocks(fixture.blocks(t)[:1]) }},
		{name: "missing certificate request", request: missing(0)},
		{name: "missing VM identity", request: missing(1)},
		{name: "missing attestation data", request: missing(2)},
		{name: "missing attestation signature", request: missing(3)},
		{name: "empty certificate request block", request: empty(0)},
		{name: "empty VM identity block", request: empty(1)},
		{name: "empty attestation data block", request: empty(2)},
		{name: "empty attestation signature block", request: empty(3)},
		{name: "truncated final block", request: func(t *testing.T) []byte {
			encoded := encodePEMBlocks(fixture.blocks(t))
			return encoded[:len(encoded)-40]
		}},
		{name: "invalid base64 in the attestation data block", request: func(t *testing.T) []byte {
			blocks := fixture.blocks(t)
			return append(encodePEMBlocks(blocks[:2]), []byte("-----BEGIN ATTESTATION DATA-----\n!!!!\n-----END ATTESTATION DATA-----\n"+string(encodePEMBlocks(blocks[3:])))...)
		}},
		{name: "duplicate attestation data", request: func(t *testing.T) []byte {
			return encodePEMBlocks(append(fixture.blocks(t), pemBlock{Type: attestationDataBlock, Bytes: fixture.data}))
		}},
		{name: "duplicate certificate request", request: func(t *testing.T) []byte {
			blocks := fixture.blocks(t)
			return encodePEMBlocks(append(blocks, blocks[0]))
		}},
		{name: "trailing data after the envelope", request: func(t *testing.T) []byte {
			return append(encodePEMBlocks(fixture.blocks(t)), []byte("not a pem block")...)
		}},
		{name: "embedded request is a different CSR", request: func(t *testing.T) []byte {
			other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			require.NoError(t, err)
			blocks := fixture.blocks(t)
			blocks[0] = pemBlock{Type: csrRequestBlock, Bytes: certificateRequestDER(t, other, fixture.vm.Name)}
			return encodePEMBlocks(blocks)
		}},

		// VM IDENTITY payloads: wrong data must never become an identity.
		{name: "VM identity that is not JSON", request: func(t *testing.T) []byte { return identityBlock(t, []byte("not json")) }},
		{name: "VM identity JSON null", request: func(t *testing.T) []byte { return identityBlock(t, []byte("null")) }},
		{name: "VM identity JSON array", request: func(t *testing.T) []byte { return identityBlock(t, []byte("[]")) }},
		{name: "VM identity JSON string", request: func(t *testing.T) []byte { return identityBlock(t, []byte(`"identity"`)) }},
		{name: "VM identity empty object", request: func(t *testing.T) []byte { return identityBlock(t, []byte("{}")) }},
		{name: "VM identity with a string instance ID", request: func(t *testing.T) []byte {
			return identityBlock(t, []byte(`{"zone":"z","id":"987654321","name":"n","project_id":1,"project_name":"p"}`))
		}},
		{name: "VM identity with a negative instance ID", request: func(t *testing.T) []byte {
			return identityBlock(t, []byte(`{"zone":"z","id":-1,"name":"n","project_id":1,"project_name":"p"}`))
		}},
		{name: "VM identity with an instance ID that overflows", request: func(t *testing.T) []byte {
			return identityBlock(t, []byte(`{"zone":"z","id":18446744073709551616,"name":"n","project_id":1,"project_name":"p"}`))
		}},
		{name: "VM identity with a fractional project number", request: func(t *testing.T) []byte {
			return identityBlock(t, []byte(`{"zone":"z","id":1,"name":"n","project_id":1.5,"project_name":"p"}`))
		}},
		{name: "VM identity without an instance ID", request: identityWith(func(v *vmIdentity) { v.ID = 0 })},
		{name: "VM identity without a project number", request: identityWith(func(v *vmIdentity) { v.ProjectNumber = 0 })},
		{name: "VM identity without a project ID", request: identityWith(func(v *vmIdentity) { v.ProjectID = "" })},
		{name: "VM identity without a zone", request: identityWith(func(v *vmIdentity) { v.Zone = "" })},
		{name: "VM identity without a name", request: identityWith(func(v *vmIdentity) { v.Name = "" })},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request := tt.request(t)
			csr, err := x509.ParseCertificateRequest(fixture.der)
			require.NoError(t, err)
			proof, err := parseAttestationEnvelope(request, csr)
			if tt.valid {
				require.NoError(t, err)
				require.Equal(t, attestationVM, proof.vm)
				require.Equal(t, fixture.data, proof.attestationData)
				require.Equal(t, fixture.sig, proof.attestationSignature)
				return
			}
			require.Error(t, err)
			require.Equal(t, attestationProof{}, proof, "a rejected envelope must not yield partial evidence")
		})
	}
}

func TestOptionalAttestationCertificateDoesNotSelectTheTrustAnchor(t *testing.T) {
	fixture := newAttestationFixture(t)
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "untrusted attestation certificate"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(4102444800, 0),
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &fixture.ak.PublicKey, fixture.ak)
	require.NoError(t, err)
	optional := pemBlock{Type: "ATTESTATION CERTIFICATE", Bytes: certificateDER}
	blocks := append(fixture.blocks(t), optional)
	request, err := x509.ParseCertificateRequest(fixture.der)
	require.NoError(t, err)
	proof, err := parseAttestationEnvelope(encodePEMBlocks(blocks), request)
	require.NoError(t, err, "the native producer may append an optional attestation certificate")
	require.NoError(t, verifyAttestationProof(proof, request, &fixture.ak.PublicKey))

	otherAK, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	require.Error(t, verifyAttestationProof(proof, request, &otherAK.PublicKey),
		"the supplied certificate must not override the authoritative Compute key")
	_, err = parseAttestationEnvelope(encodePEMBlocks(append(blocks, optional)), request)
	require.Error(t, err, "optional blocks must not bypass duplicate rejection")
}

func TestVerifyAttestationProof(t *testing.T) {
	fixture := newAttestationFixture(t)

	for _, tt := range []struct {
		name    string
		request func(t *testing.T) ([]byte, *rsa.PublicKey)
	}{
		{
			name: "requested key differs from the certified key",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				require.NoError(t, err)
				blocks := fixture.blocks(t)
				blocks[0] = pemBlock{Type: csrRequestBlock, Bytes: certificateRequestDER(t, other, fixture.vm.Name)}
				return encodePEMBlocks(blocks), &fixture.ak.PublicKey
			},
		},
		{
			name: "attestation signature from another key",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				other, err := rsa.GenerateKey(rand.Reader, 2048)
				require.NoError(t, err)
				return encodePEMBlocks(fixture.blocks(t)), &other.PublicKey
			},
		},
		{
			name: "tampered attestation signature",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				blocks := fixture.blocks(t)
				sig := append([]byte{}, fixture.sig...)
				sig[len(sig)-1] ^= 0xff
				blocks[3] = pemBlock{Type: attestationSignatureBlock, Bytes: sig}
				return encodePEMBlocks(blocks), &fixture.ak.PublicKey
			},
		},
		{
			name: "tampered attestation data",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				blocks := fixture.blocks(t)
				data := append([]byte{}, fixture.data...)
				data[len(data)-1] ^= 0xff
				blocks[2] = pemBlock{Type: attestationDataBlock, Bytes: data}
				return encodePEMBlocks(blocks), &fixture.ak.PublicKey
			},
		},
		{
			name: "non-canonical attestation data with trailing bytes",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				data := append(append([]byte{}, fixture.data...), 0x00)
				blocks := fixture.blocks(t)
				blocks[2] = pemBlock{Type: attestationDataBlock, Bytes: data}
				blocks[3] = pemBlock{Type: attestationSignatureBlock, Bytes: signAttestation(t, fixture.ak, data)}
				return encodePEMBlocks(blocks), &fixture.ak.PublicKey
			},
		},
		{
			name: "attestation data is not a certify structure",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				// Re-tag the certify structure as a quote; the signature still verifies.
				data := append([]byte{}, fixture.data...)
				data[5] = byte(uint16(tpm2.TPMSTAttestQuote) & 0xff)
				blocks := fixture.blocks(t)
				blocks[2] = pemBlock{Type: attestationDataBlock, Bytes: data}
				blocks[3] = pemBlock{Type: attestationSignatureBlock, Bytes: signAttestation(t, fixture.ak, data)}
				return encodePEMBlocks(blocks), &fixture.ak.PublicKey
			},
		},
		{
			name: "attestation data without the TPM_GENERATED magic",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				// Only the magic differs; the AK signature still verifies, so only the
				// explicit magic check can reject it.
				data := append([]byte{}, fixture.data...)
				data[0] ^= 0xff
				blocks := fixture.blocks(t)
				blocks[2] = pemBlock{Type: attestationDataBlock, Bytes: data}
				blocks[3] = pemBlock{Type: attestationSignatureBlock, Bytes: signAttestation(t, fixture.ak, data)}
				return encodePEMBlocks(blocks), &fixture.ak.PublicKey
			},
		},
		{
			name: "malformed attestation data",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				data := []byte{0xff, 0x54, 0x43}
				blocks := fixture.blocks(t)
				blocks[2] = pemBlock{Type: attestationDataBlock, Bytes: data}
				blocks[3] = pemBlock{Type: attestationSignatureBlock, Bytes: signAttestation(t, fixture.ak, data)}
				return encodePEMBlocks(blocks), &fixture.ak.PublicKey
			},
		},
		{
			name: "CSR key algorithm that cannot be certified",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
				require.NoError(t, err)
				der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
					Subject: pkix.Name{CommonName: "system:node:" + fixture.vm.Name, Organization: []string{"system:nodes"}},
				}, rsaKey)
				require.NoError(t, err)
				blocks := fixture.blocks(t)
				blocks[0] = pemBlock{Type: csrRequestBlock, Bytes: der}
				return encodePEMBlocks(blocks), &fixture.ak.PublicKey
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request, attestationKey := tt.request(t)
			// The controller parses the request's first PEM block as the CSR.
			csr, err := x509.ParseCertificateRequest(mustPEMBlock(t, request, csrRequestBlock))
			require.NoError(t, err)
			proof, err := parseAttestationEnvelope(request, csr)
			require.NoError(t, err)
			err = verifyAttestationProof(proof, csr, attestationKey)
			require.Error(t, err)
		})
	}
}

func mustPEMBlock(t *testing.T, request []byte, blockType string) []byte {
	t.Helper()
	raw := request
	for len(raw) > 0 {
		block, rest := pem.Decode(raw)
		require.NotNil(t, block)
		if block.Type == blockType {
			return block.Bytes
		}
		raw = rest
	}
	t.Fatalf("no %q block", blockType)
	return nil
}

// certifyName builds a TPM certify structure over an arbitrary Name and signs it
// with the attestation key, so a test controls exactly which TPM object the
// authoritative key vouches for.
func certifyName(t testing.TB, ak *rsa.PrivateKey, name []byte) (data, sig []byte) {
	t.Helper()
	data = tpm2.Marshal(&tpm2.TPMSAttest{
		Magic: tpm2.TPMGeneratedValue,
		Type:  tpm2.TPMSTAttestCertify,
		Attested: tpm2.NewTPMUAttest(tpm2.TPMSTAttestCertify, &tpm2.TPMSCertifyInfo{
			Name:          tpm2.TPM2BName{Buffer: name},
			QualifiedName: tpm2.TPM2BName{Buffer: append([]byte{0}, name...)},
		}),
	})
	return data, signAttestation(t, ak, data)
}

func (f *attestationFixture) proof(data, sig []byte) attestationProof {
	return attestationProof{vm: f.vm, attestationData: data, attestationSignature: sig}
}

func (f *attestationFixture) csrRequest(t testing.TB) *x509.CertificateRequest {
	t.Helper()
	csr, err := x509.ParseCertificateRequest(f.der)
	require.NoError(t, err)
	return csr
}

// TestVerifyAttestationProofRejectsAnotherTPMObject covers a proof that the
// authoritative key signed correctly, but for a TPM object other than GKE's
// imported kubelet key for the requested public key: another template, another
// name algorithm or curve label, or a corrupted Name.
func TestVerifyAttestationProofRejectsAnotherTPMObject(t *testing.T) {
	fixture := newAttestationFixture(t)
	csr := fixture.csrRequest(t)

	template := func(mutate func(*tpm2.TPMTPublic)) []byte {
		public, err := gkeKubeletPublic(fixture.key.Public())
		require.NoError(t, err)
		mutate(public)
		name, err := tpm2.ObjectName(public)
		require.NoError(t, err)
		return name.Buffer
	}
	eccParams := func(curve tpm2.TPMECCCurve, scheme tpm2.TPMIAlgECCScheme, hash tpm2.TPMIAlgHash) tpm2.TPMUPublicParms {
		details := tpm2.TPMUAsymScheme{}
		if scheme == tpm2.TPMAlgECDSA {
			details = tpm2.NewTPMUAsymScheme(tpm2.TPMAlgECDSA, &tpm2.TPMSSigSchemeECDSA{HashAlg: hash})
		}
		return tpm2.NewTPMUPublicParms(tpm2.TPMAlgECC, &tpm2.TPMSECCParms{
			Symmetric: tpm2.TPMTSymDefObject{Algorithm: tpm2.TPMAlgNull},
			Scheme:    tpm2.TPMTECCScheme{Scheme: scheme, Details: details},
			CurveID:   curve,
			KDF:       tpm2.TPMTKDFScheme{Scheme: tpm2.TPMAlgNull},
		})
	}
	canonical := template(func(*tpm2.TPMTPublic) {})
	sha256Name := func(seed string) []byte {
		sum := sha256.Sum256([]byte(seed))
		return append([]byte{0x00, 0x0b}, sum[:]...)
	}

	for _, tt := range []struct {
		name string
		tpm  []byte
		ok   bool
	}{
		{name: "control: GKE's imported kubelet key template", tpm: canonical, ok: true},

		{name: "TPM-created restricted attestation-style key", tpm: template(func(p *tpm2.TPMTPublic) {
			p.ObjectAttributes = tpm2.TPMAObject{FixedTPM: true, FixedParent: true, SensitiveDataOrigin: true, UserWithAuth: true, Restricted: true, SignEncrypt: true}
		})},
		{name: "fixed-TPM key that only differs by the FixedTPM attribute", tpm: template(func(p *tpm2.TPMTPublic) {
			p.ObjectAttributes.FixedTPM = true
		})},
		{name: "decryption key", tpm: template(func(p *tpm2.TPMTPublic) {
			p.ObjectAttributes = tpm2.TPMAObject{Decrypt: true, UserWithAuth: true}
		})},
		{name: "signing key without user auth", tpm: template(func(p *tpm2.TPMTPublic) { p.ObjectAttributes.UserWithAuth = false })},
		{name: "name algorithm SHA-384", tpm: template(func(p *tpm2.TPMTPublic) { p.NameAlg = tpm2.TPMAlgSHA384 })},
		{name: "name algorithm SHA-1", tpm: template(func(p *tpm2.TPMTPublic) { p.NameAlg = tpm2.TPMAlgSHA1 })},
		{name: "another curve label for the same point", tpm: template(func(p *tpm2.TPMTPublic) {
			p.Parameters = eccParams(tpm2.TPMECCNistP384, tpm2.TPMAlgECDSA, tpm2.TPMAlgSHA256)
		})},
		{name: "ECDSA with SHA-384", tpm: template(func(p *tpm2.TPMTPublic) {
			p.Parameters = eccParams(tpm2.TPMECCNistP256, tpm2.TPMAlgECDSA, tpm2.TPMAlgSHA384)
		})},
		{name: "no signing scheme", tpm: template(func(p *tpm2.TPMTPublic) {
			p.Parameters = eccParams(tpm2.TPMECCNistP256, tpm2.TPMAlgNull, 0)
		})},
		{name: "an authorization policy", tpm: template(func(p *tpm2.TPMTPublic) {
			p.AuthPolicy = tpm2.TPM2BDigest{Buffer: make([]byte, 32)}
		})},
		{name: "zero-padded X coordinate", tpm: template(func(p *tpm2.TPMTPublic) {
			point, err := p.Unique.ECC()
			require.NoError(t, err)
			point.X.Buffer = append([]byte{0}, point.X.Buffer...)
		})},
		{name: "another public point", tpm: template(func(p *tpm2.TPMTPublic) {
			point, err := p.Unique.ECC()
			require.NoError(t, err)
			point.Y.Buffer = append([]byte{}, point.Y.Buffer...)
			point.Y.Buffer[0] ^= 1
		})},

		{name: "an unrelated object", tpm: sha256Name("another TPM object")},
		{name: "empty name", tpm: []byte{}},
		{name: "name of zeros", tpm: make([]byte, 34)},
		{name: "name with the right digest and the SHA-1 algorithm prefix", tpm: append([]byte{0x00, 0x04}, canonical[2:]...)},
		{name: "name with a trailing byte", tpm: append(append([]byte{}, canonical...), 0)},
		{name: "truncated name", tpm: canonical[:len(canonical)-1]},
		{name: "digest without the algorithm prefix", tpm: canonical[2:]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data, sig := certifyName(t, fixture.ak, tt.tpm)
			err := verifyAttestationProof(fixture.proof(data, sig), csr, &fixture.ak.PublicKey)
			if tt.ok {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
		})
	}
}

// TestVerifyAttestationProofRejectsOtherAttestationTypes covers signed TPM
// structures that are not a certification of an object.
func TestVerifyAttestationProofRejectsOtherAttestationTypes(t *testing.T) {
	fixture := newAttestationFixture(t)
	csr := fixture.csrRequest(t)
	for _, tt := range []struct {
		name string
		tag  uint16
		ok   bool
	}{
		{name: "control: certify", tag: uint16(tpm2.TPMSTAttestCertify), ok: true},
		{name: "quote", tag: uint16(tpm2.TPMSTAttestQuote)},
		{name: "creation", tag: uint16(tpm2.TPMSTAttestCreation)},
		{name: "time", tag: uint16(tpm2.TPMSTAttestTime)},
		{name: "NV", tag: uint16(tpm2.TPMSTAttestNV)},
		{name: "NV digest", tag: uint16(tpm2.TPMSTAttestNVDigest)},
		{name: "command audit", tag: uint16(tpm2.TPMSTAttestCommandAudit)},
		{name: "session audit", tag: uint16(tpm2.TPMSTAttestSessionAudit)},
		{name: "zero tag", tag: 0},
		{name: "unassigned tag", tag: 0xffff},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := append([]byte{}, fixture.data...)
			binary.BigEndian.PutUint16(data[4:6], tt.tag)
			err := verifyAttestationProof(fixture.proof(data, signAttestation(t, fixture.ak, data)), csr, &fixture.ak.PublicKey)
			if tt.ok {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
		})
	}
}

// TestVerifyAttestationProofRejectsDegenerateInput covers empty, short and
// oversized evidence and a missing key; none may verify, panic or be skipped.
func TestVerifyAttestationProofRejectsDegenerateInput(t *testing.T) {
	fixture := newAttestationFixture(t)
	csr := fixture.csrRequest(t)
	key := &fixture.ak.PublicKey
	require.NoError(t, verifyAttestationProof(fixture.proof(fixture.data, fixture.sig), csr, key), "control")

	for _, tt := range []struct {
		name string
		data []byte
		sig  []byte
		key  *rsa.PublicKey
	}{
		{name: "nil evidence", key: key},
		{name: "empty evidence", data: []byte{}, sig: []byte{}, key: key},
		{name: "no signature", data: fixture.data, key: key},
		{name: "no data", sig: fixture.sig, key: key},
		{name: "one-byte signature", data: fixture.data, sig: []byte{0}, key: key},
		{name: "truncated signature", data: fixture.data, sig: fixture.sig[:len(fixture.sig)-1], key: key},
		{name: "signature with a trailing byte", data: fixture.data, sig: append(append([]byte{}, fixture.sig...), 0), key: key},
		{name: "all-zero signature", data: fixture.data, sig: make([]byte, len(fixture.sig)), key: key},
		{name: "signature as data", data: fixture.sig, sig: fixture.sig, key: key},
		{name: "data as signature", data: fixture.data, sig: fixture.data, key: key},
		{name: "truncated data", data: fixture.data[:len(fixture.data)-1], sig: fixture.sig, key: key},
		{name: "valid evidence without a key", data: fixture.data, sig: fixture.sig, key: nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.NotPanics(t, func() {
				require.Error(t, verifyAttestationProof(fixture.proof(tt.data, tt.sig), csr, tt.key))
			})
		})
	}
}

// TestVerifyAttestationProofRequiresRSASSAPKCS1v15SHA256 covers valid RSA
// signatures made with another hash or padding: GKE's AK signs PKCS#1 v1.5 SHA-256.
func TestVerifyAttestationProofRequiresRSASSAPKCS1v15SHA256(t *testing.T) {
	fixture := newAttestationFixture(t)
	csr := fixture.csrRequest(t)
	pkcs1 := func(hash crypto.Hash) []byte {
		h := hash.New()
		h.Write(fixture.data)
		sig, err := rsa.SignPKCS1v15(rand.Reader, fixture.ak, hash, h.Sum(nil))
		require.NoError(t, err)
		return sig
	}
	pss := func(hash crypto.Hash) []byte {
		h := hash.New()
		h.Write(fixture.data)
		sig, err := rsa.SignPSS(rand.Reader, fixture.ak, hash, h.Sum(nil), nil)
		require.NoError(t, err)
		return sig
	}
	require.NoError(t, verifyAttestationProof(fixture.proof(fixture.data, pkcs1(crypto.SHA256)), csr, &fixture.ak.PublicKey), "control")
	for name, sig := range map[string][]byte{
		"PKCS#1 v1.5 SHA-1":   pkcs1(crypto.SHA1),
		"PKCS#1 v1.5 SHA-384": pkcs1(crypto.SHA384),
		"PKCS#1 v1.5 SHA-512": pkcs1(crypto.SHA512),
		"PSS SHA-256":         pss(crypto.SHA256),
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, verifyAttestationProof(fixture.proof(fixture.data, sig), csr, &fixture.ak.PublicKey))
		})
	}
}

// TestVerifyAttestationProofRejectsEverySingleBitFlip changes each bit of the
// signed data and of the signature on its own. None may verify.
func TestVerifyAttestationProofRejectsEverySingleBitFlip(t *testing.T) {
	fixture := newAttestationFixture(t)
	csr := fixture.csrRequest(t)
	for i := range fixture.data {
		for bit := 0; bit < 8; bit++ {
			data := append([]byte{}, fixture.data...)
			data[i] ^= 1 << bit
			require.Error(t, verifyAttestationProof(fixture.proof(data, fixture.sig), csr, &fixture.ak.PublicKey), "data byte %d bit %d", i, bit)
		}
	}
	for i := range fixture.sig {
		for bit := 0; bit < 8; bit++ {
			sig := append([]byte{}, fixture.sig...)
			sig[i] ^= 1 << bit
			require.Error(t, verifyAttestationProof(fixture.proof(fixture.data, sig), csr, &fixture.ak.PublicKey), "signature byte %d bit %d", i, bit)
		}
	}
}

func TestParseAttestationKey(t *testing.T) {
	fixture := newAttestationFixture(t)
	valid := fixture.attestationKeyPEM(t)

	for _, tt := range []struct {
		name string
		key  string
	}{
		{name: "valid RSA attestation key", key: valid},
		{name: "empty key", key: ""},
		{name: "not PEM", key: "not a key"},
		{name: "EC key", key: func() string {
			der, err := x509.MarshalPKIXPublicKey(&fixture.key.PublicKey)
			require.NoError(t, err)
			return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
		}()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			key, err := parseAttestationKey(tt.key)
			if tt.name == "valid RSA attestation key" {
				require.NoError(t, err)
				require.NotNil(t, key)
				return
			}
			require.Error(t, err)
		})
	}
}

func TestGKEKubeletPublic(t *testing.T) {
	t.Parallel()

	// Fixed public points and manually encoded TPMT_PUBLIC/Name fixtures, generated
	// independently of gkeKubeletPublic and go-tpm's encoder from the historical GKE
	// template cited in attestation.go. Each curve includes a leading-zero coordinate.
	data, err := os.ReadFile("testdata/gke-public-vectors.json")
	require.NoError(t, err)
	var vectors []struct {
		Name    string `json:"name"`
		Curve   string `json:"curve"`
		Point   string `json:"point"`
		Public  string `json:"public"`
		TPMName string `json:"tpmName"`
	}
	require.NoError(t, json.Unmarshal(data, &vectors))
	curves := map[string]elliptic.Curve{
		"P-224": elliptic.P224(), "P-256": elliptic.P256(),
		"P-384": elliptic.P384(), "P-521": elliptic.P521(),
	}
	for _, vector := range vectors {
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()
			point, err := hex.DecodeString(vector.Point)
			require.NoError(t, err)
			key, err := ecdsa.ParseUncompressedPublicKey(curves[vector.Curve], point)
			require.NoError(t, err)
			public, err := gkeKubeletPublic(key)
			require.NoError(t, err)
			require.Equal(t, vector.Public, hex.EncodeToString(tpm2.Marshal(public)))
			name, err := tpm2.ObjectName(public)
			require.NoError(t, err)
			require.Equal(t, vector.TPMName, hex.EncodeToString(tpm2.Marshal(name)))
		})
	}
}

func TestGKEKubeletPublicRejectsRSA(t *testing.T) {
	t.Parallel()
	_, err := gkeKubeletPublic(&rsa.PublicKey{})
	require.Error(t, err, "only ECDSA keys can be certified this way")
}
