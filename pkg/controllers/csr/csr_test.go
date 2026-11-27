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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	certv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/instance"
)

const (
	bootstrapUser = "kubelet-nodepool-bootstrap"
	tpmBootstrap  = "kubelet-bootstrap"
	nodePoolOrg   = "gke:nodepool:name:karpenter-default"
	instanceOrg   = "gke:instance:123456789:us-central1-a:987654321"
)

func requestPEM(t *testing.T, template *x509.CertificateRequest) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func nodeRequest(t *testing.T, nodeName string) []byte {
	return requestPEM(t, &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   "system:node:" + nodeName,
			Organization: []string{"system:nodes", nodePoolOrg, instanceOrg},
		},
	})
}

func kubeletCSR(request []byte, username string, groups ...string) *certv1.CertificateSigningRequest {
	return &certv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "csr-test"},
		Spec: certv1.CertificateSigningRequestSpec{
			Request:    request,
			SignerName: certv1.KubeAPIServerClientKubeletSignerName,
			Usages:     []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageKeyEncipherment, certv1.UsageClientAuth},
			Username:   username,
			Groups:     groups,
		},
	}
}

func bootstrapCSR(request []byte) *certv1.CertificateSigningRequest {
	return kubeletCSR(request, bootstrapUser, nodePoolOrg, "system:authenticated")
}

func nodeCSR(request []byte, nodeName string) *certv1.CertificateSigningRequest {
	return kubeletCSR(request, "system:node:"+nodeName, "system:nodes", nodePoolOrg, instanceOrg, "system:authenticated")
}

// launchedNodeClaim returns a launched claim for a VM with this name and numeric
// incarnation ID.
func launchedNodeClaim(name, instanceName string, numericID uint64) karpv1.NodeClaim {
	nc := karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: map[string]string{v1alpha1.AnnotationInstanceNumericID: strconv.FormatUint(numericID, 10)},
		},
		Spec: karpv1.NodeClaimSpec{NodeClassRef: &karpv1.NodeClassReference{
			Group: apis.Group,
			Kind:  "GCENodeClass",
			Name:  "default",
		}},
	}
	nc.Status.ProviderID = "gce://my-project/us-central1-a/" + instanceName
	nc.StatusConditions().SetTrue(karpv1.ConditionTypeLaunched)
	return nc
}

func registeredNodeClaim(name, instanceName string, numericID uint64) karpv1.NodeClaim {
	nc := launchedNodeClaim(name, instanceName, numericID)
	nc.Status.NodeName = instanceName
	nc.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
	return nc
}

// identityProviderFunc adapts a function to the IdentityProvider seam.
type identityProviderFunc func(ctx context.Context, zone, name string) (*instance.InstanceIncarnation, error)

func (f identityProviderFunc) GetIncarnation(ctx context.Context, zone, name string) (*instance.InstanceIncarnation, error) {
	return f(ctx, zone, name)
}

// shieldedPolicyFunc adapts a function to the ShieldedPolicyProvider seam.
type shieldedPolicyFunc func(ctx context.Context) (bool, error)

func (f shieldedPolicyFunc) ShieldedNodesEnabled(ctx context.Context) (bool, error) {
	return f(ctx)
}

// newTestController wires a controller for scenarios that must not depend on the
// cluster Shielded Nodes policy: reading it fails the test and the read.
func newTestController(t *testing.T, api kubernetes.Interface, identity IdentityProvider) *Controller {
	t.Helper()
	return NewController(api, identity, shieldedPolicyFunc(func(context.Context) (bool, error) {
		t.Error("the TPM bootstrap and self-renewal paths must not read the cluster Shielded Nodes policy")
		return false, errors.New("unexpected Shielded Nodes policy read")
	}))
}

func TestAuthorizeKubeletClientCSR(t *testing.T) {
	for _, tc := range []struct {
		name string
		// csr returns the request under test.
		csr func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest
		// claims returns the NodeClaims the controller lists.
		claims       func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim
		wantApproved bool
	}{
		{
			name: "initial enrollment of a launched claim",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return bootstrapCSR(nodeRequest(t, attestationVM.Name))
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				return []karpv1.NodeClaim{claimForFixture(t, "pending")}
			},
			wantApproved: true,
		},
		{
			name: "initial enrollment with the GKE TPM bootstrap requester",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return kubeletCSR(nodeRequest(t, attestationVM.Name), tpmBootstrap, nodePoolOrg, "system:authenticated")
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				return []karpv1.NodeClaim{claimForFixture(t, "pending")}
			},
			wantApproved: true,
		},
		{
			name: "bootstrap request for a registered claim",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return bootstrapCSR(nodeRequest(t, attestationVM.Name))
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				nc := claimForFixture(t, "registered")
				nc.Status.NodeName = attestationVM.Name
				nc.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
				return []karpv1.NodeClaim{nc}
			},
		},
		{
			name: "initial enrollment of a deleting claim",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return bootstrapCSR(nodeRequest(t, attestationVM.Name))
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				nc := claimForFixture(t, "pending")
				now := metav1.Now()
				nc.DeletionTimestamp = &now
				return []karpv1.NodeClaim{nc}
			},
		},
		{
			name: "initial enrollment of an unlaunched claim",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return bootstrapCSR(nodeRequest(t, attestationVM.Name))
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				nc := claimForFixture(t, "pending")
				nc.Status.Conditions = nil
				return []karpv1.NodeClaim{nc}
			},
		},
		{
			name: "bootstrap request for a claim registered as another node",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return bootstrapCSR(nodeRequest(t, attestationVM.Name))
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				nc := claimForFixture(t, "registered")
				nc.Status.NodeName = "another-node"
				nc.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
				return []karpv1.NodeClaim{nc}
			},
		},
		{
			name: "request for a node with no NodeClaim",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return bootstrapCSR(nodeRequest(t, "gke-cluster-default-pool-1234"))
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim { return nil },
		},
		{
			name: "request for a NodeClaim of another provider",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return bootstrapCSR(nodeRequest(t, attestationVM.Name))
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				nc := claimForFixture(t, "pending")
				nc.Spec.NodeClassRef.Group = "another.provider.example"
				return []karpv1.NodeClaim{nc}
			},
		},
		{
			name: "request for a claim in another zone",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return bootstrapCSR(nodeRequest(t, attestationVM.Name))
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				nc := claimForFixture(t, "pending")
				nc.Status.ProviderID = "gce://my-project/us-central1-b/" + attestationVM.Name
				return []karpv1.NodeClaim{nc}
			},
		},
		{
			name: "ordinary self renewal",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return nodeCSR(nodeRequest(t, attestationVM.Name), attestationVM.Name)
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				nc := claimForFixture(t, "registered")
				nc.Status.NodeName = attestationVM.Name
				nc.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
				return []karpv1.NodeClaim{nc}
			},
			wantApproved: true,
		},
		{
			name: "self renewal while the registered claim drains",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return nodeCSR(nodeRequest(t, attestationVM.Name), attestationVM.Name)
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				nc := claimForFixture(t, "registered")
				nc.Status.NodeName = attestationVM.Name
				nc.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
				now := metav1.Now()
				nc.DeletionTimestamp = &now
				return []karpv1.NodeClaim{nc}
			},
			wantApproved: true,
		},
		{
			name: "self renewal without the recorded VM incarnation",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return nodeCSR(nodeRequest(t, attestationVM.Name), attestationVM.Name)
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				nc := claimForFixture(t, "registered")
				delete(nc.Annotations, v1alpha1.AnnotationInstanceNumericID)
				nc.Status.NodeName = attestationVM.Name
				nc.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
				return []karpv1.NodeClaim{nc}
			},
			wantApproved: true,
		},
		{
			name: "node requesting a registered node's identity",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return nodeCSR(nodeRequest(t, attestationVM.Name), "another-node")
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				nc := claimForFixture(t, "registered")
				nc.Status.NodeName = attestationVM.Name
				nc.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
				return []karpv1.NodeClaim{nc}
			},
		},
		{
			name: "self renewal for an unregistered claim",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return nodeCSR(nodeRequest(t, attestationVM.Name), attestationVM.Name)
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				return []karpv1.NodeClaim{claimForFixture(t, "pending")}
			},
		},
		{
			name: "bootstrap request for a node pool group the requester is not in",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return kubeletCSR(nodeRequest(t, attestationVM.Name), bootstrapUser, "system:authenticated")
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				return []karpv1.NodeClaim{claimForFixture(t, "pending")}
			},
		},
		{
			name: "requester that is not a supported bootstrap or node identity",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return kubeletCSR(nodeRequest(t, attestationVM.Name), "system:serviceaccount:default:attacker", nodePoolOrg, "system:authenticated")
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				return []karpv1.NodeClaim{claimForFixture(t, "pending")}
			},
		},
		{
			name: "kubelet serving signer",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				csr := bootstrapCSR(nodeRequest(t, attestationVM.Name))
				csr.Spec.SignerName = certv1.KubeletServingSignerName
				return csr
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				return []karpv1.NodeClaim{claimForFixture(t, "pending")}
			},
		},
		{
			name: "unsupported usage",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				csr := bootstrapCSR(nodeRequest(t, attestationVM.Name))
				csr.Spec.Usages = append(csr.Spec.Usages, certv1.UsageServerAuth)
				return csr
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				return []karpv1.NodeClaim{claimForFixture(t, "pending")}
			},
		},
		{
			name: "subject alternative names",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return bootstrapCSR(requestPEM(t, &x509.CertificateRequest{
					Subject:  pkix.Name{CommonName: "system:node:" + attestationVM.Name, Organization: []string{"system:nodes"}},
					DNSNames: []string{attestationVM.Name},
				}))
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				return []karpv1.NodeClaim{claimForFixture(t, "pending")}
			},
		},
		{
			name: "request that is not PEM",
			csr: func(t *testing.T, fixture *attestationFixture) *certv1.CertificateSigningRequest {
				return bootstrapCSR([]byte("not a csr"))
			},
			claims: func(t *testing.T, fixture *attestationFixture) []karpv1.NodeClaim {
				return []karpv1.NodeClaim{claimForFixture(t, "pending")}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newAttestationFixture(t)
			identity := identityProviderFunc(func(context.Context, string, string) (*instance.InstanceIncarnation, error) {
				t.Error("the TPM bootstrap and self-renewal paths must not read the live VM")
				return nil, errors.New("unexpected live VM read")
			})
			api, double := newKubeAPIDouble(t)
			controller := newTestController(t, api, identity)
			controller.client = &csrReadClient{request: tc.csr(t, fixture), claims: tc.claims(t, fixture)}

			_, err := controller.Reconcile(context.Background())
			require.NoError(t, err)
			require.Empty(t, double.recordedReviews(), "these paths are approved without a SubjectAccessReview")
			approvals := double.recordedApprovals()
			if !tc.wantApproved {
				require.Empty(t, approvals)
				return
			}
			require.Len(t, approvals, 1)
			require.Equal(t, certv1.CertificateApproved, approvals[0].Status.Conditions[0].Type)
			require.Equal(t, "KarpenterAutoApprover", approvals[0].Status.Conditions[0].Reason)
		})
	}
}

func claimForFixture(t *testing.T, name string) karpv1.NodeClaim {
	t.Helper()
	return launchedNodeClaim(name, attestationVM.Name, attestationVM.ID)
}

func TestValidateRenewal(t *testing.T) {
	claim := registeredNodeClaim("target", attestationVM.Name, attestationVM.ID)
	groups := sets.New[string]("system:nodes", nodePoolOrg, instanceOrg)
	subject := kubeletClientSubject{nodeName: attestationVM.Name, nodePoolOrg: nodePoolOrg, instanceOrg: instanceOrg}
	require.NoError(t, validateRenewal(subject, groups, []karpv1.NodeClaim{claim}))

	// The instance organization belongs to the requester in the authenticated groups.
	require.Error(t, validateRenewal(subject, sets.New[string]("system:nodes"), []karpv1.NodeClaim{claim}))
	// The claim must be registered, not merely launched.
	require.Error(t, validateRenewal(subject, groups, []karpv1.NodeClaim{claimForFixture(t, "pending")}))
}

func TestParseKubeletClientSubjectRejectsDuplicateOrganizations(t *testing.T) {
	_, err := parseKubeletClientSubject("system:node:node-a", []string{"system:nodes", nodePoolOrg, nodePoolOrg})
	require.Error(t, err)
	_, err = parseKubeletClientSubject("system:node:node-a", []string{"system:nodes", "system:masters"})
	require.Error(t, err)
	_, err = parseKubeletClientSubject("node-a", []string{"system:nodes"})
	require.Error(t, err)
}

func TestParseProviderID(t *testing.T) {
	project, zone, instanceName := parseProviderID("gce://project/us-central1-a/vm-1")
	require.Equal(t, "project", project)
	require.Equal(t, "us-central1-a", zone)
	require.Equal(t, "vm-1", instanceName)
	for _, id := range []string{"aws:///us-east-1a/i-123", "gce://project/vm-1", "gce:///us-central1-a/vm-1", "gce://project//vm-1", "gce://project/us-central1-a/", ""} {
		project, zone, instanceName = parseProviderID(id)
		require.Empty(t, project)
		require.Empty(t, zone)
		require.Empty(t, instanceName)
	}
}

func TestRecordedIncarnation(t *testing.T) {
	claim := claimForFixture(t, "pending")
	id, err := recordedIncarnation(&claim)
	require.NoError(t, err)
	require.Equal(t, attestationVM.ID, id)

	for _, tt := range []struct {
		name       string
		annotation func(*karpv1.NodeClaim)
	}{
		{name: "missing", annotation: func(nc *karpv1.NodeClaim) { delete(nc.Annotations, v1alpha1.AnnotationInstanceNumericID) }},
		{name: "empty", annotation: func(nc *karpv1.NodeClaim) { nc.Annotations[v1alpha1.AnnotationInstanceNumericID] = "" }},
		{name: "zero", annotation: func(nc *karpv1.NodeClaim) { nc.Annotations[v1alpha1.AnnotationInstanceNumericID] = "0" }},
		{name: "not a number", annotation: func(nc *karpv1.NodeClaim) { nc.Annotations[v1alpha1.AnnotationInstanceNumericID] = "vm-1" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			claim := claimForFixture(t, "pending")
			tt.annotation(&claim)
			_, err := recordedIncarnation(&claim)
			require.Error(t, err)
		})
	}
}
