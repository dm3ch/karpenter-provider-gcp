#!/usr/bin/env python3
import base64
import hashlib
import json
import os
from pathlib import Path

bundle = json.loads(Path(os.environ['BUNDLE']).read_text())
statement = json.loads(base64.b64decode(bundle['dsseEnvelope']['payload']))
version = os.environ['tag_semver']
expected = {f'karpenter-{version}.tgz', f'karpenter-crd-{version}.tgz'}
subjects = statement['subject']
assert len(subjects) == 2
assert {Path(subject['name']).name for subject in subjects} == expected
for subject in subjects:
    archive = Path(os.environ['CHARTS_DIR']) / Path(subject['name']).name
    assert subject['digest']['sha256'] == hashlib.sha256(archive.read_bytes()).hexdigest()
print('PASS: attestation contains only the two freshly built chart archives with their exact digests')
