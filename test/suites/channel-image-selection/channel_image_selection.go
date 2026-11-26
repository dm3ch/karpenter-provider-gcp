/*
Copyright 2026 The CloudPilot AI Authors.

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

package channelimageselection

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/imagefamily"
	"github.com/karpenter-gcp/karpenter-provider-gcp/test/pkg/environment"
)

// familyChannelTestCase describes a channel/version-based image selection scenario.
type familyChannelTestCase struct {
	family    string
	osVersion string // set for family Ubuntu
	channel   string // set for channel tests
	version   string // set for version tests
}

var env *environment.Environment
var _ = BeforeEach(func() { env = environment.Current() })

var _ = DescribeTable("ChannelImageSelection", Label("suite:channel-image-selection"),
	func(ctx SpecContext, tc familyChannelTestCase) {
		runChannelImageSelectionTest(ctx, tc)
	},
	// family: ContainerOptimizedOS, channel: stable — exercises the full channel resolution
	// pipeline: GetServerConfig → ResolveVersionForChannel → resolveExactBuildCOSImage.
	Entry("COS / channel: stable / amd64 / on-demand",
		familyChannelTestCase{family: gcpv1alpha1.ImageFamilyContainerOptimizedOS, channel: gcpv1alpha1.ImageChannelStable},
		SpecTimeout(15*time.Minute)),
	// family: Ubuntu, osVersion: 24.04, version: latest — exercises the Ubuntu image resolution
	// path, which uses different naming conventions and different GCP projects from COS
	// (ubuntu-os-gke-cloud before GKE 1.36, gke-ubuntu-node-images from 1.36).
	Entry("Ubuntu osVersion 24.04 / version: latest / amd64 / on-demand",
		familyChannelTestCase{family: gcpv1alpha1.ImageFamilyUbuntu, osVersion: "24.04", version: "latest"},
		SpecTimeout(15*time.Minute)),
	// osVersion default follows GKE's default Ubuntu release for the cluster version (22.04 before
	// 1.32.1, 24.04 after); the expectation is derived from the cluster version.
	Entry("Ubuntu osVersion default / version: latest / amd64 / on-demand",
		familyChannelTestCase{family: gcpv1alpha1.ImageFamilyUbuntu, osVersion: gcpv1alpha1.ImageOSVersionDefault, version: "latest"},
		SpecTimeout(15*time.Minute)),
	// Omitting osVersion means default, so it must resolve the same image as osVersion default.
	Entry("Ubuntu osVersion omitted / version: latest / amd64 / on-demand",
		familyChannelTestCase{family: gcpv1alpha1.ImageFamilyUbuntu, version: "latest"},
		SpecTimeout(15*time.Minute)),
	// osVersion 26.04 skips itself when GKE publishes no clean 26.04 image for the cluster's
	// minor (it does not for 1.36, which this suite runs on today), so it starts running by
	// itself once the E2E cluster is on a minor that has 26.04 images (1.37 and later).
	Entry("Ubuntu osVersion 26.04 / version: latest / amd64 / on-demand",
		familyChannelTestCase{family: gcpv1alpha1.ImageFamilyUbuntu, osVersion: "26.04", version: "latest"},
		SpecTimeout(15*time.Minute)),
	// The deprecated per-release family must keep resolving to the same image.
	Entry("Ubuntu2404 (deprecated) / version: latest / amd64 / on-demand",
		familyChannelTestCase{family: gcpv1alpha1.ImageFamilyUbuntu2404, version: "latest"},
		SpecTimeout(15*time.Minute)),
)

func runChannelImageSelectionTest(ctx context.Context, tc familyChannelTestCase) {
	if tc.channel != "" {
		// A cluster can run a Kubernetes minor that a slower channel has not promoted yet.
		env.SkipIfChannelLacksClusterMinor(ctx, tc.channel)
	}

	if tc.osVersion == "26.04" && env.ResolveUbuntuVersion(ctx, "2604") == "" {
		Skip("GKE publishes no clean Ubuntu 26.04 image for this cluster's Kubernetes minor")
	}
	prefix := environment.TestPrefix(karpv1.ArchitectureAmd64, karpv1.CapacityTypeOnDemand, "channel-img")
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] family=%s osVersion=%s channel=%s version=%s nodePool=%s\n",
		tc.family, tc.osVersion, tc.channel, tc.version, name)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if provisionedNodeName != "" {
			Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
		}
	})

	switch {
	case tc.channel != "":
		env.CreateNodeClassWithFamilyChannel(ctx, name, tc.family, tc.channel)
	case tc.osVersion != "":
		env.CreateNodeClassWithUbuntuOSVersion(ctx, name, tc.osVersion, tc.version)
	default:
		env.CreateNodeClassWithFamilyVersion(ctx, name, tc.family, tc.version)
	}
	env.WaitForNodeClassReady(ctx, name)
	nodeClass, err := env.GetNodeClass(ctx, name)
	Expect(err).NotTo(HaveOccurred())
	Expect(nodeClass.Status.Images).NotTo(BeEmpty())
	expectedRelease := nodeClass.Status.Images[0].KubernetesVersion
	Expect(expectedRelease).NotTo(BeEmpty(), "selected images must carry their bootstrap release")
	for _, image := range nodeClass.Status.Images {
		Expect(image.KubernetesVersion).To(Equal(expectedRelease))
	}

	// Verify the resolved image version is consistent with the requested selector.
	sourceImages := env.NodeClassSourceImages(ctx, name)
	Expect(sourceImages).NotTo(BeEmpty(),
		"no images in status.images after NodeClass became Ready (family=%s channel=%s version=%s)",
		tc.family, tc.channel, tc.version)

	expectResolvedImage(ctx, tc, sourceImages)

	env.CreateNodePool(ctx, name, name, environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
	})
	env.WaitForNodePoolReady(ctx, name)
	env.CreateDeployment(ctx, name, name, name, karpv1.ArchitectureAmd64)

	env.WaitForNodeClaimLaunched(ctx, name)
	pod := env.WaitForRunningPod(ctx, name)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())

	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	provisionedNodeName = node.Name

	Expect(environment.IsNodeReady(node)).To(BeTrue(), "node %s is not Ready", node.Name)
	Expect(node.Labels[karpv1.NodeRegisteredLabelKey]).To(Equal("true"))
	Expect(node.Labels[karpv1.NodePoolLabelKey]).To(Equal(name))
	Expect(node.Labels[karpv1.CapacityTypeLabelKey]).To(Equal(karpv1.CapacityTypeOnDemand))
	Expect(strings.TrimPrefix(node.Status.NodeInfo.KubeletVersion, "v")).To(Equal(expectedRelease),
		"running kubelet must match the selected image release, not the bootstrap source pool")
}

// expectResolvedImage verifies the resolved image is consistent with the requested selector.
func expectResolvedImage(ctx context.Context, tc familyChannelTestCase, sourceImages []string) {
	switch {
	case tc.channel != "" && tc.family == gcpv1alpha1.ImageFamilyContainerOptimizedOS:
		// For channel-based COS selection, verify the resolved image's embedded GKE build
		// matches what ResolveVersionForChannel returns for the given channel.
		expectedGKEVersion := env.GetChannelVersion(ctx, tc.channel)
		k8sKey, build, ok := imagefamily.ParseGKEVersion(expectedGKEVersion)
		Expect(ok).To(BeTrue(), "could not parse GKE version %q returned by server config", expectedGKEVersion)
		expectedSubstr := fmt.Sprintf("gke-%s-gke%s-", k8sKey, build)
		Expect(sourceImages[0]).To(ContainSubstring(expectedSubstr),
			"resolved COS image does not match channel %s version %s (expected pattern %s in %s)",
			tc.channel, expectedGKEVersion, expectedSubstr, sourceImages[0])
	case tc.version == "latest":
		expectResolvedUbuntuImage(ctx, tc, sourceImages[0])
	}
}

// expectResolvedUbuntuImage verifies `version: latest` Ubuntu selections resolve the release the
// selector asks for. Specs of other families and pins have no expectation here.
func expectResolvedUbuntuImage(ctx context.Context, tc familyChannelTestCase, image string) {
	switch {
	case tc.osVersion == "26.04":
		// A fixed 26.04 selection must resolve the newest clean 26.04 image for the cluster's minor.
		expectUbuntuImage(ctx, image, "2604", env.ResolveUbuntuVersion(ctx, "2604"))
	case tc.family == gcpv1alpha1.ImageFamilyUbuntu2404 || tc.osVersion == "24.04":
		// A fixed 24.04 selection must resolve the newest 24.04 image for the cluster's minor.
		expectUbuntuImage(ctx, image, "2404", env.ResolveCurrentUbuntuVersion(ctx))
	case tc.osVersion == gcpv1alpha1.ImageOSVersionDefault || (tc.family == gcpv1alpha1.ImageFamilyUbuntu && tc.osVersion == ""):
		// default and an omitted osVersion follow GKE's default release for the cluster version
		// (22.04 before 1.32.1, 24.04 after), not always 24.04.
		release := env.ExpectedUbuntuDefaultRelease(ctx)
		version := env.ResolveUbuntuVersion(ctx, release)
		Expect(version).NotTo(BeEmpty(), "no Ubuntu %s image found for the cluster's K8s minor", release)
		expectUbuntuImage(ctx, image, release, version)
	}
}

// expectUbuntuImage checks that a resolved Ubuntu image is what `latest` selects for a release: the
// per-patch image GKE publishes for the cluster master's exact version when there is one (GKE 1.36
// and later; its base build follows GKE's patch cadence), otherwise the newest plain image. Both
// expectations are looked up independently of the controller.
func expectUbuntuImage(ctx context.Context, image, release, newestPlainVersion string) {
	if perPatch := env.ResolveUbuntuPerPatchImage(ctx, release); perPatch != "" {
		Expect(image).To(HaveSuffix("/"+perPatch),
			"GKE publishes per-patch image %s for the cluster's exact version, but %s was selected", perPatch, image)
		return
	}
	Expect(image).To(ContainSubstring("ubuntu-gke-"+release+"-"),
		"no per-patch image exists for the cluster's version, so a plain Ubuntu %s image is expected: %s", release, image)
	Expect(image).To(ContainSubstring(newestPlainVersion),
		"resolved plain Ubuntu %s image does not contain the newest version %s", release, newestPlainVersion)
}
