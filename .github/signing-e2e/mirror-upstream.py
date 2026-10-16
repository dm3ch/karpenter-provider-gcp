#!/usr/bin/env python3
"""Build the explicitly authorized upstream-to-fork Pages mirror."""
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import sys
from urllib.error import HTTPError
from urllib.request import urlopen

BASE = 'https://cloudpilot-ai.github.io/karpenter-provider-gcp/'
INDEX_SHA256 = '1558e7a2af5aacbabc5b7c80c714f9204dea99a230da02af92b0d8dd7be1c766'
destination = Path(sys.argv[1])
destination.mkdir(parents=True, exist_ok=False)


def fetch(name):
    with urlopen(BASE + name, timeout=60) as response:
        return response.read()


index = fetch('index.yaml')
assert hashlib.sha256(index).hexdigest() == INDEX_SHA256, 'Upstream index changed since validation; re-snapshot before publishing'
entries = json.loads((Path(__file__).parent / 'upstream-history.json').read_text())


def copy(entry):
    name = entry['urls'][0]
    assert '/' not in name and name.endswith('.tgz')
    content = fetch(name)
    assert hashlib.sha256(content).hexdigest() == entry['digest'], name
    try:
        fetch(name + '.prov')
    except HTTPError as error:
        assert error.code == 404
    else:
        raise AssertionError('Unsigned baseline changed: ' + name)
    (destination / name).write_bytes(content)
    return name


with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
    copied = list(pool.map(copy, entries))
(destination / 'index.yaml').write_bytes(index)
# Authorization covers the broken baseline reset, never later signed releases.
fork = 'https://dm3ch.github.io/karpenter-provider-gcp/'
query = '?signing-e2e-mirror=' + os.environ.get('GITHUB_RUN_ID', 'preflight')
with urlopen(fork + 'index.yaml' + query, timeout=60) as response:
    current = response.read()
allowed = {INDEX_SHA256, '9d7580801f466a6cd51918c8e021452dbed27fb9d1b7b2c5a075aa6a87357f7f'}
assert hashlib.sha256(current).hexdigest() in allowed, 'Fork Pages advanced beyond the authorized baseline; refusing to erase history'
try:
    with urlopen(fork + 'signed-charts.txt' + query, timeout=60) as response:
        assert not response.read().strip(), 'Fork now contains signed history; refusing to reset it'
except HTTPError as error:
    assert error.code == 404
# Do not mirror Artifact Hub's upstream repository ownership identifier into a fork.
print(f'Validated and mirrored {len(copied)} unsigned archives plus the exact upstream index; destination reset guard passed')
