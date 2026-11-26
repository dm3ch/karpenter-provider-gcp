/*
Copyright 2025 The CloudPilot AI Authors.

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

package imagefamily

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/patrickmn/go-cache"
	"google.golang.org/api/compute/v1"
	v1 "k8s.io/api/core/v1"
	k8sversion "k8s.io/apimachinery/pkg/util/version"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	versionprovider "github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/version"
)

const (
	// ubuntuLegacyImageProject publishes GKE Ubuntu node images for GKE versions before 1.36.
	ubuntuLegacyImageProject = "ubuntu-os-gke-cloud"
	// ubuntuImageProject publishes them for GKE 1.36 and later.
	ubuntuImageProject = "gke-ubuntu-node-images"
	// ubuntuImageProjectMinor is the first Kubernetes 1.x minor served from ubuntuImageProject.
	ubuntuImageProjectMinor = 36
)

// ubuntuPinnedVersionRe validates a pinned image version (vYYYYMMDD, optionally with a release letter).
var ubuntuPinnedVersionRe = regexp.MustCompile(`^v\d{8}[a-z]?$`)

// ubuntuRelease holds the clean-name allowlists for one Ubuntu release. Anchoring on the version
// end rejects variant builds (-tpu, -cgroupsv1, -linux64k, -64k, -hwe, -acs, ...) while allowing
// a trailing date letter (v20251218a).
type ubuntuRelease struct {
	amd64, arm64 *regexp.Regexp
	// hint is appended to resolution failures to explain a known catalog limit.
	hint string
}

// newUbuntuRelease builds the allowlists for a release. 2204 amd64 names carry no arch token.
func newUbuntuRelease(id string, amd64ArchToken bool, hint string) ubuntuRelease {
	amd64Token := ""
	if amd64ArchToken {
		amd64Token = "amd64-"
	}
	return ubuntuRelease{
		amd64: regexp.MustCompile(fmt.Sprintf(`^ubuntu-gke-%s-\d+-\d+-%sv\d+[a-z]?$`, id, amd64Token)),
		arm64: regexp.MustCompile(fmt.Sprintf(`^ubuntu-gke-%s-\d+-\d+-arm64-v\d+[a-z]?$`, id)),
		hint:  hint,
	}
}

// ubuntuOSVersions lists the releases in ubuntuReleases as osVersion values ("22.04"), sorted.
func ubuntuOSVersions() []string {
	versions := make([]string, 0, len(ubuntuReleases))
	for release := range ubuntuReleases {
		versions = append(versions, release[:2]+"."+release[2:])
	}
	slices.Sort(versions)
	return versions
}

const (
	// ubuntuPerPatchMaxPages bounds the catalog scan for per-patch images to the newest pages
	// (500 images each); a missing image is never worth crawling the whole catalog.
	ubuntuPerPatchMaxPages = 8
	// ubuntuPerPatchBatchWindow is how far past the newest matching per-patch image the scan keeps
	// reading to see sibling base builds of the same GKE version, which are published together.
	ubuntuPerPatchBatchWindow = 24 * time.Hour
	// Per-patch image names are immutable and encode the GKE version, so hits are cached long; misses
	// (not yet published) are retried sooner.
	ubuntuPerPatchHitTTL  = time.Hour
	ubuntuPerPatchMissTTL = 10 * time.Minute

	// ubuntuReleaseDefault is the osVersion "default": the release GKE uses for the cluster version.
	ubuntuReleaseDefault = "default"
	// ubuntuDefaultVerifiedMinor is the highest Kubernetes 1.x minor whose GKE default Ubuntu
	// release has been verified: GKE's release notes document no change to the Ubuntu default since
	// 1.32.1 (checked through 1.37). Above it, osVersion "default" uses the fallback release of
	// ubuntuDefaults, with a warning. TestUbuntuDefaultVerifiedCoversMaxK8sVersion requires it to be
	// at least the minor of versionprovider.MaxK8sVersion, so raising that maximum requires
	// re-verifying the default first.
	ubuntuDefaultVerifiedMinor = 37
)

// ubuntuDefaults lists the release GKE uses for Ubuntu node images by default, from each Kubernetes
// version on. Source: GKE release notes, January 2025: "Starting with GKE version 1.32.1-gke.1002000,
// the default OS image for Ubuntu is updated from Ubuntu 22.04 to Ubuntu 24.04"
// (https://docs.cloud.google.com/kubernetes-engine/docs/release-notes). Before that, 22.04 was the
// default. GKE publishes no machine-readable mapping and has announced no later change.
//
// Exactly one row is the fallback: GKE's current documented default, used for cluster versions above
// the verified range, where GKE may have moved its default without this table knowing. Cluster
// versions below the first row use that first row.
var ubuntuDefaults = []struct {
	from, release string
	fallback      bool
}{
	{from: "1.28.0", release: "2204"},
	{from: "1.32.1", release: "2404", fallback: true},
}

// ubuntuDefaultRelease returns the release token GKE uses by default for kubernetesVersion, and
// whether that is verified. Versions outside the verified range resolve to the fallback release
// (above) or the oldest row (below) with verified=false; callers should say so.
func ubuntuDefaultRelease(kubernetesVersion string) (release string, verified bool, err error) {
	parsed, err := k8sversion.ParseGeneric(kubernetesVersion)
	if err != nil {
		return "", false, fmt.Errorf("parsing Kubernetes version for the default Ubuntu release: %w", err)
	}
	verified = !parsed.LessThan(k8sversion.MustParseGeneric(versionprovider.MinK8sVersion)) &&
		parsed.Major() == 1 && parsed.Minor() <= ubuntuDefaultVerifiedMinor
	if parsed.Major() > 1 || parsed.Minor() > ubuntuDefaultVerifiedMinor {
		for _, d := range ubuntuDefaults {
			if d.fallback {
				return d.release, false, nil
			}
		}
	}
	release = ubuntuDefaults[0].release
	for _, d := range ubuntuDefaults {
		if parsed.AtLeast(k8sversion.MustParseGeneric(d.from)) {
			release = d.release
		}
	}
	return release, verified, nil
}

// forCluster returns the resolver for this release, resolving "default" to the release GKE uses
// for kubernetesVersion.
func (u *Ubuntu) forCluster(ctx context.Context, kubernetesVersion string) (*Ubuntu, error) {
	if u.release != ubuntuReleaseDefault {
		return u, nil
	}
	release, verified, err := ubuntuDefaultRelease(kubernetesVersion)
	if err != nil {
		return nil, err
	}
	if !verified {
		log.FromContext(ctx).Info("the default Ubuntu release is not verified for this Kubernetes version; using the fallback release",
			"kubernetesVersion", kubernetesVersion, "osVersion", release[:2]+"."+release[2:], "verifiedThroughMinor", ubuntuDefaultVerifiedMinor)
	}
	resolved := *u
	resolved.release, resolved.defaulted = release, true
	return &resolved, nil
}

// ubuntuReleases is keyed by the release token embedded in image names.
var ubuntuReleases = map[string]ubuntuRelease{
	"2204": newUbuntuRelease("2204", false, "GKE publishes Ubuntu 22.04 node images only through Kubernetes 1.31"),
	"2404": newUbuntuRelease("2404", true, ""),
	"2604": newUbuntuRelease("2604", true, ""),
}

// Ubuntu resolves GKE Ubuntu images from the image project that serves the cluster's Kubernetes
// minor: ubuntu-os-gke-cloud before GKE 1.36, gke-ubuntu-node-images from 1.36.
// The release field selects the Ubuntu release: a key of ubuntuReleases, or ubuntuReleaseDefault
// to use the release GKE defaults to for the cluster's Kubernetes version.
type Ubuntu struct {
	computeService  *compute.Service
	versionProvider versionprovider.Provider
	release         string
	defaulted       bool // release was chosen by osVersion "default"
	// perPatchCache remembers per-patch image lookups across resolutions; nil disables caching.
	perPatchCache *cache.Cache
}

func (u *Ubuntu) imagePrefix() string {
	return "ubuntu-gke-" + u.release
}

// architecture requirements this provider resolves an image for, in output order.
var ubuntuArchitectures = []string{OSArchAMD64Requirement, OSArchARM64Requirement}

func (u *Ubuntu) ResolveImages(ctx context.Context, version string) (Images, error) {
	if version != "latest" && !ubuntuPinnedVersionRe.MatchString(version) {
		return nil, &imageResolutionError{msg: fmt.Sprintf(
			"invalid Ubuntu version %q: must be 'latest' or 'vYYYYMMDD' with an optional release letter (e.g. 'v20260416', 'v20251218a')", version)}
	}

	kubernetesVersion, err := bootstrapVersion(ctx, u.versionProvider)
	if err != nil {
		return nil, err
	}
	u, err = u.forCluster(ctx, kubernetesVersion)
	if err != nil {
		return nil, err
	}
	project, prefix, err := u.imageScope(kubernetesVersion)
	if err != nil {
		return nil, err
	}
	plain, err := u.findImages(ctx, version, project, prefix)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to resolve Ubuntu GKE image from image project")
		return Images{}, err
	}
	// Prefer the per-patch image GKE publishes for the exact master version: it carries that
	// release's components preloaded, which is what GKE node pools boot, and starts nodes much
	// faster. No per-patch image means the plain image, but a failed lookup is an error: silently
	// switching to the plain image, whose base build can be newer, would drift running nodes.
	perPatch, err := u.findPerPatch(ctx, project, kubernetesVersion, version)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to look up per-patch Ubuntu images")
		return Images{}, err
	}

	return u.imagesFor(plain, perPatch, project, kubernetesVersion, version, bootDiskGiB(ctx))
}

// imagesFor picks the image for each architecture, the per-patch one when there is one and the
// NodeClass boot disk (bootGiB, 0 when unset) can hold it: GCE rejects a disk smaller than the
// image, and the per-patch image is larger than the plain one. A missing architecture (including a
// pinned date absent for one arch) fails resolution instead of producing a single-arch set that
// reads as ImagesReady=True.
func (u *Ubuntu) imagesFor(plain, perPatch map[string]*compute.Image, project, kubernetesVersion, version string, bootGiB int64) (Images, error) {
	ret := Images{}
	for _, arch := range ubuntuArchitectures {
		img, ok := perPatch[arch]
		if ok && bootGiB > 0 && img.DiskSizeGb > bootGiB {
			ok = false
		}
		if !ok {
			img, ok = plain[arch]
		}
		if !ok {
			return nil, u.noImageError(arch, version, project, kubernetesVersion)
		}
		ret = append(ret, Image{
			SourceImage:       fmt.Sprintf("projects/%s/global/images/%s", project, img.Name),
			KubernetesVersion: kubernetesVersion,
			Requirements:      ubuntuRequirementsForArch(arch),
		})
	}
	return ret, nil
}

// noImageError explains why no image exists for arch, naming the release and project, whether the
// release came from osVersion default, and any known catalog limit of the release.
func (u *Ubuntu) noImageError(arch, version, project, kubernetesVersion string) error {
	msg := fmt.Sprintf("no %s %s image found for version %q in %s", u.imagePrefix(), arch, version, project)
	if u.defaulted {
		msg += fmt.Sprintf("; osVersion default selected Ubuntu %s.%s for Kubernetes %s", u.release[:2], u.release[2:], kubernetesVersion)
	}
	if hint := ubuntuReleases[u.release].hint; hint != "" {
		msg += "; " + hint
	}
	return &imageResolutionError{msg: msg}
}

// findImages returns the newest usable image for each architecture ("latest"), or the exact-date
// image for a pin. scanImages visits newest first, so the first usable match wins.
func (u *Ubuntu) findImages(ctx context.Context, version, project, prefix string) (map[string]*compute.Image, error) {
	found := make(map[string]*compute.Image, len(ubuntuArchitectures))
	err := scanImages(ctx, u.computeService, project, func(img *compute.Image) bool {
		if !strings.HasPrefix(img.Name, prefix) || (version != "latest" && !strings.HasSuffix(img.Name, "-"+version)) {
			return false
		}
		for _, arch := range ubuntuArchitectures {
			if found[arch] == nil && u.isUsableForArch(img, arch) {
				found[arch] = img
			}
		}
		return len(found) == len(ubuntuArchitectures)
	})
	if err != nil {
		return nil, fmt.Errorf("listing Ubuntu GKE images in %s: %w", project, err)
	}
	return found, nil
}

// findPerPatch returns, per architecture, the newest usable per-patch image GKE publishes for the
// exact GKE version of the cluster master (a pin selects its base build), or nothing when the
// project, version or release has none. Per-patch images are named
// gke-<version>-gke<build>-ubuntu-<release>-<minor>-<arch>-vDATE-c-pre and exist only in
// ubuntuImageProject (GKE 1.36 and later). The lookup follows the catalog-quota rules of the other
// scans: no server-side filter, newest first, partial responses, a page bound, and a cache.
func (u *Ubuntu) findPerPatch(ctx context.Context, project, kubernetesVersion, version string) (map[string]*compute.Image, error) {
	if project != ubuntuImageProject {
		return nil, nil
	}
	k8sKey, build, ok := ParseGKEVersion(kubernetesVersion)
	if !ok {
		return nil, nil
	}
	cacheKey := strings.Join([]string{project, u.release, kubernetesVersion, version}, "|")
	if u.perPatchCache != nil {
		if hit, ok := u.perPatchCache.Get(cacheKey); ok {
			found := hit.(map[string]*compute.Image)
			stillUsable, err := u.stillUsable(ctx, project, found)
			if err != nil {
				return nil, err
			}
			if stillUsable {
				return found, nil
			}
			u.perPatchCache.Delete(cacheKey) // deleted or deprecated since it was cached: scan again
		}
	}
	scan := newPerPatchScan(k8sKey, build, u.release, version)
	if err := scanImagesPages(ctx, u.computeService, project, ubuntuPerPatchMaxPages, scan.visit); err != nil {
		return nil, fmt.Errorf("listing per-patch Ubuntu GKE images in %s: %w", project, err)
	}
	if u.perPatchCache != nil {
		ttl := ubuntuPerPatchHitTTL
		if len(scan.found) == 0 {
			ttl = ubuntuPerPatchMissTTL
		}
		u.perPatchCache.Set(cacheKey, scan.found, ttl)
	}
	return scan.found, nil
}

// stillUsable reports whether every cached per-patch image still exists and is usable, so an image
// Google deleted or deprecated within the cache lifetime is not kept selected.
func (u *Ubuntu) stillUsable(ctx context.Context, project string, found map[string]*compute.Image) (bool, error) {
	for _, img := range found {
		current, err := lookupImage(ctx, u.computeService, project, img.Name)
		if err != nil {
			return false, fmt.Errorf("checking per-patch Ubuntu image %s in %s: %w", img.Name, project, err)
		}
		if current == nil || !imageUsable(current) {
			return false, nil
		}
	}
	return true, nil
}

// perPatchScan selects, while images are visited newest first, the newest base build of the
// per-patch image of one GKE version for each architecture.
type perPatchScan struct {
	names   map[string]*regexp.Regexp // by architecture; group 1 is the base date
	version string                    // "latest" or a pinned base date
	found   map[string]*compute.Image
	dates   map[string]string
	floor   time.Time // creation time of the oldest image selected so far
}

func newPerPatchScan(k8sKey, build, release, version string) *perPatchScan {
	s := &perPatchScan{
		names:   make(map[string]*regexp.Regexp, len(ubuntuArchitectures)),
		version: version,
		found:   make(map[string]*compute.Image, len(ubuntuArchitectures)),
		dates:   make(map[string]string, len(ubuntuArchitectures)),
	}
	for _, arch := range ubuntuArchitectures {
		s.names[arch] = regexp.MustCompile(fmt.Sprintf(`^gke-%s-gke%s-ubuntu-%s-\d+-\d+-%s-(v\d{8}[a-z]?)-c-pre$`,
			regexp.QuoteMeta(k8sKey), regexp.QuoteMeta(build), release, arch))
	}
	return s
}

// visit reports true once the scan is past the batch that published the version's images.
func (s *perPatchScan) visit(img *compute.Image) bool {
	created, timeErr := time.Parse(time.RFC3339, img.CreationTimestamp)
	if s.pastBatch(created, timeErr) {
		return true
	}
	if imageUsable(img) {
		for _, arch := range ubuntuArchitectures {
			s.consider(arch, img, created, timeErr)
		}
	}
	return false
}

// pastBatch reports whether every architecture has an image and created is beyond the window in
// which sibling base builds of the same GKE version are published together.
func (s *perPatchScan) pastBatch(created time.Time, timeErr error) bool {
	return len(s.found) == len(ubuntuArchitectures) && (timeErr != nil || created.Before(s.floor.Add(-ubuntuPerPatchBatchWindow)))
}

// consider selects img for arch when it is a per-patch image of this version and base build
// request, and newer than the one selected so far.
func (s *perPatchScan) consider(arch string, img *compute.Image, created time.Time, timeErr error) {
	m := s.names[arch].FindStringSubmatch(img.Name)
	if m == nil || (s.version != "latest" && m[1] != s.version) {
		return
	}
	if s.found[arch] == nil || m[1] > s.dates[arch] { // a later date letter sorts after the bare date
		s.found[arch], s.dates[arch] = img, m[1]
	}
	if timeErr == nil && (s.floor.IsZero() || created.Before(s.floor)) {
		s.floor = created
	}
}

// isUsableForArch reports whether img is a READY, non-deprecated, clean image of this release for arch.
func (u *Ubuntu) isUsableForArch(img *compute.Image, arch string) bool {
	release, ok := ubuntuReleases[u.release]
	if !ok || !imageUsable(img) {
		return false
	}
	if arch == OSArchARM64Requirement {
		return release.arm64.MatchString(img.Name)
	}
	return release.amd64.MatchString(img.Name)
}

// ubuntuRequirementsForArch builds the scheduling requirements for a resolved image.
func ubuntuRequirementsForArch(arch string) scheduling.Requirements {
	if arch == OSArchARM64Requirement {
		return scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchARM64Requirement),
			scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, v1.NodeSelectorOpDoesNotExist))
	}
	return scheduling.NewRequirements(
		scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchAMD64Requirement))
}

// imageScope returns the image project and name prefix for the cluster's Kubernetes minor.
// GKE publishes Ubuntu images for 1.36+ in ubuntuImageProject and for earlier minors in
// ubuntuLegacyImageProject; the projects are not mixed or used as fallbacks for each other.
func (u *Ubuntu) imageScope(kubernetesVersion string) (project, prefix string, err error) {
	parsed, err := k8sversion.ParseGeneric(kubernetesVersion)
	if err != nil {
		return "", "", fmt.Errorf("parsing Ubuntu image version: %w", err)
	}
	project = ubuntuLegacyImageProject
	if parsed.Major() > 1 || parsed.Minor() >= ubuntuImageProjectMinor {
		project = ubuntuImageProject
	}
	return project, fmt.Sprintf("%s-%d-%d", u.imagePrefix(), parsed.Major(), parsed.Minor()), nil
}

// ubuntuImageProjects lists the projects that serve GKE Ubuntu images.
var ubuntuImageProjects = []string{ubuntuLegacyImageProject, ubuntuImageProject}

// ubuntuBuildRe parses the Ubuntu GKE image names that denote the same OS build: the plain image
// (ubuntu-gke-<release>-<minor>-[<arch>-]vDATE; 22.04 amd64 has no arch token) and the per-patch image
// (gke-<version>-gke<build>-ubuntu-<release>-<minor>-<arch>-vDATE-c-pre).
var (
	ubuntuPlainBuildRe    = regexp.MustCompile(`^ubuntu-gke-(\d{4})-(\d+-\d+)-(?:(amd64|arm64)-)?(v\d{8}[a-z]?)$`)
	ubuntuPerPatchBuildRe = regexp.MustCompile(`^gke-\d+-gke\d+-ubuntu-(\d{4})-(\d+-\d+)-(amd64|arm64)-(v\d{8}[a-z]?)-c-pre$`)
	imageSourceRe         = regexp.MustCompile(`projects/([^/]+)/global/images/([^/]+)$`)
)

// ubuntuBuild identifies the OS build behind an Ubuntu GKE image reference as release, image minor,
// architecture and base date. Packaging differences (plain or per-patch with preloaded components,
// either Ubuntu image project) are not part of it.
func ubuntuBuild(image string) (string, bool) {
	m := imageSourceRe.FindStringSubmatch(image)
	if m == nil || !slices.Contains(ubuntuImageProjects, m[1]) {
		return "", false
	}
	for _, re := range []*regexp.Regexp{ubuntuPlainBuildRe, ubuntuPerPatchBuildRe} {
		if parts := re.FindStringSubmatch(m[2]); parts != nil {
			arch := parts[3]
			if arch == "" {
				arch = OSArchAMD64Requirement
			}
			return strings.Join([]string{parts[1], parts[2], arch, parts[4]}, "|"), true
		}
	}
	return "", false
}

// SameImage reports whether imageID, the boot image of a running node, is sourceImage from the
// NodeClass status. Ubuntu images count as the same when they are the same OS build (release,
// minor, architecture and base date): GKE mirrors images under identical names in both Ubuntu image
// projects, and a per-patch image is the plain image of that build with the GKE release preloaded.
// Neither a project move nor switching to the per-patch image replaces running nodes; only a
// different base build does.
func SameImage(imageID, sourceImage string) bool {
	if strings.HasSuffix(imageID, sourceImage) {
		return true
	}
	have, ok := ubuntuBuild(imageID)
	want, ok2 := ubuntuBuild(sourceImage)
	return ok && ok2 && have == want
}
