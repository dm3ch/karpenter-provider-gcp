#!/usr/bin/env python3
"""Independently verify published fork artifacts, including previous signed charts."""
import json
import os
from pathlib import Path
import shutil
import subprocess
from urllib.error import HTTPError

from history import fetch

repo = os.environ['GITHUB_REPOSITORY']
ref = os.environ['GITHUB_REF']
identity = 'https://github.com/' + os.environ['GITHUB_WORKFLOW_REF']
image = os.environ['IMAGE']
base = f'https://{repo.split("/")[0]}.github.io/{repo.split("/")[1]}/'
version = ref.removeprefix('refs/tags/v')
root = Path(os.environ['RUNNER_TEMP']) / 'verification'
root.mkdir()


def command(args, output=None, succeeds=True):
    result = subprocess.run(args, capture_output=True)
    if output:
        (root / output).write_bytes(result.stdout)
    assert (result.returncode == 0) == succeeds, ' '.join(args) + '\n' + result.stderr.decode()
    return result.stdout


verify = ['cosign', 'verify', '--new-bundle-format=false', '--experimental-oci11=false',
          '--certificate-oidc-issuer', 'https://token.actions.githubusercontent.com']
command(verify + ['--certificate-identity', identity, image], output='image-signature.json')
command(verify + ['--certificate-identity', 'https://github.com/untrusted/not-this-workflow@refs/heads/nope', image], succeeds=False)
command(['gh', 'attestation', 'verify', 'oci://' + image, '--repo', repo,
         '--signer-workflow', repo + '/.github/workflows/release-build.yaml', '--source-ref', ref,
         '--format', 'json'], output='image-provenance.json')
for platform in ('index', 'linux/amd64', 'linux/arm64'):
    args = ['cosign', 'download', 'sbom']
    if platform != 'index':
        args += ['--platform', platform]
    data = command(args + [image], output=platform.replace('/', '-') + '.spdx.json')
    sbom = json.loads(data)
    assert sbom['spdxVersion'].startswith('SPDX-')
    assert sbom.get('packages'), platform
print('PASS: published image signature, wrong-identity rejection, exact-source build provenance and all three SBOMs')

signed = dict(line.split() for line in fetch(base, 'signed-charts.txt').decode().splitlines())
assert f'karpenter-{version}.tgz' in signed
assert f'karpenter-crd-{version}.tgz' in signed
fingerprints = set(signed.values())
for fingerprint in fingerprints:
    public = root / (fingerprint + '.asc')
    public.write_bytes(fetch(base, f'keys/{fingerprint}.asc'))
    listing = command(['gpg', '--batch', '--with-colons', '--show-keys', str(public)]).decode()
    actual = next(line.split(':')[9] for line in listing.splitlines() if line.startswith('fpr:'))
    assert actual == fingerprint
    result = subprocess.run(['gpg', '--batch', '--dearmor'], input=public.read_bytes(), capture_output=True, check=True)
    (root / (fingerprint + '.gpg')).write_bytes(result.stdout)
for archive, fingerprint in signed.items():
    local = root / archive
    local.write_bytes(fetch(base, archive))
    (root / (archive + '.prov')).write_bytes(fetch(base, archive + '.prov'))
    command(['helm', 'verify', str(local), '--keyring', str(root / (fingerprint + '.gpg'))])
    chart = 'karpenter-crd' if archive.startswith('karpenter-crd-') else 'karpenter'
    old_version = archive.removeprefix(chart + '-').removesuffix('.tgz')
    command(['gh', 'attestation', 'verify', str(local), '--repo', repo,
             '--signer-workflow', repo + '/.github/workflows/release-build.yaml',
             '--source-ref', 'refs/tags/v' + old_version, '--format', 'json'], output=archive + '.provenance.json')
    print('PASS: downloaded chart signature and exact-source build provenance:', archive, fingerprint)

current = root / f'karpenter-{version}.tgz'
tamper_directory = root / 'tampered'
tamper_directory.mkdir()
tampered = tamper_directory / current.name
shutil.copy(current, tampered)
shutil.copy(root / (current.name + '.prov'), tamper_directory / (current.name + '.prov'))
tamper_verify = ['helm', 'verify', str(tampered), '--keyring', str(root / (signed[current.name] + '.gpg'))]
command(tamper_verify)  # The unchanged same-name copy must verify before mutation.
tampered.write_bytes(tampered.read_bytes() + b'tampered downloaded copy')
tamper_result = subprocess.run(tamper_verify, capture_output=True, text=True)
assert tamper_result.returncode != 0
assert 'sha256 sum does not match' in tamper_result.stderr, tamper_result.stderr
try:
    fetch(base, f'unexpected-{version}.tgz')
except HTTPError as error:
    assert error.code == 404
else:
    raise AssertionError('Unexpected pre-existing source archive was published')
if version.endswith('.2'):
    first_version = version[:-1] + '1'
    assert f'karpenter-{first_version}.tgz' in signed
    assert f'karpenter-crd-{first_version}.tgz' in signed
    assert len(fingerprints) == 2, 'Test key rotation was not exercised'
    old_fingerprint = signed[f'karpenter-{first_version}.tgz']
    assert old_fingerprint != signed[current.name]
    command(['helm', 'verify', str(current), '--keyring', str(root / (old_fingerprint + '.gpg'))], succeeds=False)
    print('PASS: first signed version and both old/new keys survived; wrong signing key rejected')
print('PASS: tampered downloaded chart rejected; unexpected source archive absent')
(root / 'verification-summary.json').write_text(json.dumps({
    'image': image, 'workflowIdentity': identity, 'ref': ref,
    'signedCharts': signed, 'publicFingerprints': sorted(fingerprints),
    'sbomPlatforms': ['index', 'linux/amd64', 'linux/arm64'],
}, indent=2))
