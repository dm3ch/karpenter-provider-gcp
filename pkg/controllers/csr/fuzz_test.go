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

// Fuzz targets for code that reads bytes a requester controls. `go test` runs every
// seed as an ordinary unit test and `make fuzz` (part of `make ci`) explores each
// target for a bounded time. Run one target for longer with, for example:
//
//	go test ./pkg/controllers/csr -run '^$' -fuzz '^FuzzParseKubeletClientRequest$' -fuzztime 1m
//
// Each target states a property: arbitrary input never panics, and invalid input is
// never accepted. Seeds hold a valid input and its empty, truncated and corrupted
// forms.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/require"
)

// FuzzParseKubeletClientRequest checks that an arbitrary request field never
// panics and that an accepted request is a kubelet client CSR for a named node
// without subject alternative names.
func FuzzParseKubeletClientRequest(f *testing.F) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(f, err)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   nodeUserPrefix + "node-1",
			Organization: []string{nodesGroup, nodePoolOrgPrefix + "pool", instanceOrgPrefix + "123456789:us-central1-a:987654321"},
		},
	}, key)
	require.NoError(f, err)
	valid := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})

	f.Add(valid)
	f.Add([]byte{})
	f.Add([]byte("not a csr"))
	f.Add(valid[:len(valid)/2])
	f.Add(append([]byte("junk\n"), valid...))
	f.Add(append(append([]byte{}, valid...), valid...))
	f.Add(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: []byte("not DER")}))
	f.Add(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	f.Fuzz(func(t *testing.T, request []byte) {
		subject, csr, err := parseKubeletClientRequest(request)
		if err != nil {
			require.Nil(t, csr)
			require.Equal(t, kubeletClientSubject{}, subject, "a rejected request must not yield a subject")
			return
		}
		require.NotNil(t, csr)
		require.NotEmpty(t, subject.nodeName)
		require.Empty(t, csr.DNSNames)
		require.Empty(t, csr.IPAddresses)
		require.Empty(t, csr.EmailAddresses)
		require.Empty(t, csr.URIs)
	})
}
