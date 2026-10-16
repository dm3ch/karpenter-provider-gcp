#!/usr/bin/env python3
"""Fork-only before-deploy and after-deploy chart history checks."""
import argparse
import hashlib
import json
import os
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import urljoin, urlparse
from urllib.request import urlopen

import yaml


def fetch(base, name, optional=False):
    try:
        url = urljoin(base.rstrip('/') + '/', name)
        if os.environ.get('GITHUB_RUN_ID'):
            url += '?signing-e2e=' + os.environ['GITHUB_RUN_ID']
        with urlopen(url, timeout=60) as response:
            return response.read()
    except HTTPError as error:
        if optional and error.code == 404:
            return None
        raise


def sha(data):
    return hashlib.sha256(data).hexdigest()


def entries(data):
    index = yaml.safe_load(data)
    return {(name, entry['version']): entry for name, group in index['entries'].items() for entry in group}


def basename(entry):
    assert len(entry['urls']) == 1
    url = entry['urls'][0]
    assert not urlparse(url).scheme and '/' not in url, f'Expected local chart URL: {url}'
    return url


def snapshot(base, index_path, output):
    index = Path(index_path).read_bytes()
    signed = fetch(base, 'signed-charts.txt', optional=True) or b''
    known = dict(line.split() for line in signed.decode().splitlines())
    result = {'entries': [], 'keys': {}, 'signed': known}
    for (name, version), entry in entries(index).items():
        filename = basename(entry)
        content = fetch(base, filename)
        assert sha(content) == entry['digest'], f'Baseline archive digest mismatch: {filename}'
        provenance = fetch(base, filename + '.prov', optional=filename not in known)
        result['entries'].append({
            'name': name, 'version': version, 'filename': filename,
            'digest': entry['digest'], 'created': entry['created'], 'urls': entry['urls'],
            'provenance': sha(provenance) if provenance is not None else None,
        })
    for fingerprint in set(known.values()):
        result['keys'][fingerprint] = sha(fetch(base, f'keys/{fingerprint}.asc'))
    Path(output).write_text(json.dumps(result, indent=2, default=str))
    print(f"Snapshotted {len(result['entries'])} old archives, {len(known)} signed archives, {len(result['keys'])} keys")


def check(snapshot_path, directory=None, base=None):
    old = json.loads(Path(snapshot_path).read_text())
    read = (lambda name: (Path(directory) / name).read_bytes()) if directory else (lambda name: fetch(base, name))
    current = entries(read('index.yaml'))
    signed = dict(line.split() for line in read('signed-charts.txt').decode().splitlines())
    for entry in old['entries']:
        filename = entry['filename']
        now = current[(entry['name'], entry['version'])]
        for field in ('digest', 'created', 'urls'):
            assert str(now[field]) == str(entry[field]), f'Historical {field} changed: {filename}'
        assert basename(now) == filename
        assert sha(read(filename)) == entry['digest'], f'Historical chart lost or changed: {filename}'
        if entry['provenance'] is not None:
            assert sha(read(filename + '.prov')) == entry['provenance'], f'Provenance lost or changed: {filename}'
        else:
            assert filename not in signed, f'Unsigned history was retroactively signed: {filename}'
            if directory:
                assert not (Path(directory) / (filename + '.prov')).exists()
            else:
                assert fetch(base, filename + '.prov', optional=True) is None
    for archive, fingerprint in old['signed'].items():
        assert signed[archive] == fingerprint, f'Signed history changed: {archive}'
    for fingerprint, digest in old['keys'].items():
        assert sha(read(f'keys/{fingerprint}.asc')) == digest, f'Public key lost or changed: {fingerprint}'
    print(f"PASS: preserved {len(old['entries'])} chart bytes/index records, {len(old['signed'])} provenance records and {len(old['keys'])} old keys")


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('mode', choices=['snapshot', 'check-local', 'check-published'])
    parser.add_argument('--url')
    parser.add_argument('--index')
    parser.add_argument('--snapshot', required=True)
    parser.add_argument('--directory')
    args = parser.parse_args()
    if args.mode == 'snapshot':
        snapshot(args.url, args.index, args.snapshot)
    elif args.mode == 'check-local':
        check(args.snapshot, directory=args.directory)
    else:
        check(args.snapshot, base=args.url)
