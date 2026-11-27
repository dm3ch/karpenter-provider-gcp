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

// Fuzz targets for the attestation code, which reads bytes a requester controls: the
// attestation envelope, the TPM structures inside it and the Compute-returned key.
// `go test` runs every seed as an ordinary unit test and `make fuzz` explores each
// target for a bounded time. Run one target for longer with, for example:
//
//	go test ./pkg/controllers/csr -run '^$' -fuzz '^FuzzVerifyAttestationProof$' -fuzztime 1m
//
// Each target states a property: arbitrary input never panics, and invalid input is
// never accepted. Seeds hold a valid input and its empty, truncated and corrupted
// forms.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/google/go-tpm/tpm2"
	"github.com/stretchr/testify/require"
)

// FuzzVerifyAttestationProof checks that no evidence other than the genuine pair
// verifies and that arbitrary evidence never panics.
func FuzzVerifyAttestationProof(f *testing.F) {
	fixture := newAttestationFixture(f)
	csr := fixture.csrRequest(f)
	f.Add(fixture.data, fixture.sig)
	f.Add(fixture.data, fixture.sig[:len(fixture.sig)-1])
	f.Add(fixture.data[:len(fixture.data)-1], fixture.sig)
	f.Add([]byte{}, []byte{})
	f.Add(fixture.sig, fixture.data)
	f.Fuzz(func(t *testing.T, data, sig []byte) {
		err := verifyAttestationProof(fixture.proof(data, sig), csr, &fixture.ak.PublicKey)
		if string(data) == string(fixture.data) && string(sig) == string(fixture.sig) {
			require.NoError(t, err)
			return
		}
		require.Error(t, err)
	})
}

// FuzzParseAttestationEnvelope checks that arbitrary requests never panic and
// that an accepted envelope yields complete evidence and no partial result on error.
func FuzzParseAttestationEnvelope(f *testing.F) {
	fixture := newAttestationFixture(f)
	csr := fixture.csrRequest(f)
	valid := encodePEMBlocks(fixture.blocks(f))
	f.Add(valid)
	f.Add([]byte{})
	f.Add([]byte("-----BEGIN CERTIFICATE REQUEST-----\n"))
	f.Add(append([]byte("junk\n"), valid...))
	f.Add(valid[:len(valid)/2])
	f.Add(append(append([]byte{}, valid...), valid...))
	f.Fuzz(func(t *testing.T, request []byte) {
		proof, err := parseAttestationEnvelope(request, csr)
		if err != nil {
			require.Equal(t, attestationProof{}, proof)
			return
		}
		require.NotEmpty(t, proof.attestationData)
		require.NotEmpty(t, proof.attestationSignature)
		require.NotZero(t, proof.vm.ID)
		require.NotZero(t, proof.vm.ProjectNumber)
		require.NotEmpty(t, proof.vm.Name)
		require.NotEmpty(t, proof.vm.Zone)
		require.NotEmpty(t, proof.vm.ProjectID)
	})
}

// FuzzParseAttestationKey checks that an arbitrary Compute-returned key string
// never panics and only an RSA public key is accepted.
func FuzzParseAttestationKey(f *testing.F) {
	fixture := newAttestationFixture(f)
	valid := fixture.attestationKeyPEM(f)
	f.Add(valid)
	f.Add("")
	f.Add("-----BEGIN PUBLIC KEY-----\n")
	f.Add(strings.ReplaceAll(valid, "PUBLIC KEY", "PRIVATE KEY"))
	f.Add(valid[:len(valid)/2])
	f.Add(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("not DER")})))
	f.Fuzz(func(t *testing.T, pemEncoded string) {
		key, err := parseAttestationKey(pemEncoded)
		if err != nil {
			require.Nil(t, key)
			return
		}
		require.NotNil(t, key)
		require.NotNil(t, key.N)
		require.Positive(t, key.E)
	})
}

// FuzzGKEKubeletPublic checks that every valid public point of an accepted curve
// converts to a TPM object whose Name is a SHA-256 Name, and that coordinates are
// minimally encoded as GKE's template requires.
func FuzzGKEKubeletPublic(f *testing.F) {
	curves := []elliptic.Curve{elliptic.P224(), elliptic.P256(), elliptic.P384(), elliptic.P521()}
	for i, curve := range curves {
		key, err := ecdsa.GenerateKey(curve, rand.Reader)
		require.NoError(f, err)
		point, err := key.PublicKey.Bytes()
		require.NoError(f, err)
		f.Add(uint8(i), point)
	}
	f.Add(uint8(0), []byte{})
	f.Add(uint8(1), []byte{4})
	f.Fuzz(func(t *testing.T, curve uint8, point []byte) {
		key, err := ecdsa.ParseUncompressedPublicKey(curves[int(curve)%len(curves)], point)
		if err != nil {
			return // only an on-curve point can come from a parsed CSR
		}
		public, err := gkeKubeletPublic(key)
		require.NoError(t, err)
		name, err := tpm2.ObjectName(public)
		require.NoError(t, err)
		require.Len(t, name.Buffer, 2+32)
		coordinates, err := public.Unique.ECC()
		require.NoError(t, err)
		for _, coordinate := range [][]byte{coordinates.X.Buffer, coordinates.Y.Buffer} {
			require.False(t, len(coordinate) > 0 && coordinate[0] == 0, "coordinates must be minimally encoded")
		}
	})
}
