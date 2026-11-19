#!/usr/bin/env python3
"""Build the standard-library forge and verify its ELF architecture and size."""
import hashlib
import json
import os
from pathlib import Path
import struct
import subprocess

root = Path(__file__).resolve().parent
subprocess.run(['gofmt', '-w', *[str(p) for p in sorted(root.glob('*.go'))]], cwd=root, check=True)
subprocess.run(['go', 'test', './...'], cwd=root, check=True)
results = {}
for name, goos, goarch in [('forge-local', 'android', 'arm64'), ('forge-linux-amd64', 'linux', 'amd64')]:
    env = dict(os.environ, CGO_ENABLED='0', GOOS=goos, GOARCH=goarch)
    ldflags = '-s -w -buildid='
    if goos == 'android':
        # Match this Termux host's actual page size; no unnecessary 64-KiB gaps.
        ldflags += f' -R={os.sysconf("SC_PAGE_SIZE")}'
    subprocess.run(['go', 'build', '-trimpath', '-buildvcs=false', '-ldflags=' + ldflags, '-o', name, '.'], cwd=root, env=env, check=True)
    data = (root / name).read_bytes()
    assert data[:4] == b'\x7fELF' and data[4:6] == b'\x02\x01', name
    machine = struct.unpack_from('<H', data, 18)[0]
    assert machine == (183 if goarch == 'arm64' else 62), (name, machine)
    phoff = struct.unpack_from('<Q', data, 32)[0]
    phsize, phcount = struct.unpack_from('<HH', data, 54)
    headers = [struct.unpack_from('<I', data, phoff + phsize * n)[0] for n in range(phcount)]
    if goos == 'linux':
        assert 3 not in headers, 'Linux binary unexpectedly has a dynamic interpreter'
        assert 2 not in headers, 'Linux binary unexpectedly has PT_DYNAMIC'
    assert len(data) < 7.8 * 1024 * 1024, (name, len(data))
    results[name] = dict(bytes=len(data), sha256=hashlib.sha256(data).hexdigest(), goos=goos, goarch=goarch, elf_machine=machine, program_headers=headers)
print(json.dumps(results, indent=2))
