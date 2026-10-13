"""Real byte/encoding/archive fixtures for independent filesystem acceptance."""
from __future__ import annotations
import hashlib
import io
import os
from pathlib import Path
import stat
import tarfile
import zipfile


def digest(content: bytes) -> str:
    return hashlib.sha256(content).hexdigest()


def prepare_files(root: Path, *, transfer_bytes: int = 256 * 1024):
    root.mkdir(parents=True, exist_ok=True)
    text = '人和 AI 编辑同一文件。\nUnicode: 松绿 🌿 café\n'
    fixtures = {
        'utf8.txt': text.encode('utf-8'),
        'gb18030.txt': text.encode('gb18030'),
        'big5.txt': '繁體中文檔案\n'.encode('big5'),
        'latin1.txt': 'café\n'.encode('latin-1'),
        'binary.dat': bytes(range(256)) * 16,
        'nested/子目录/便签.txt': '真实目录内容\n'.encode(),
        '.hidden.txt': b'hidden fixture\n',
        'tail.log': b''.join(f'line {i:05d}\n'.encode() for i in range(10000)),
        'download.bin': (bytes(range(256)) * ((transfer_bytes + 255) // 256))[:transfer_bytes],
    }
    for name, payload in fixtures.items():
        path = root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(payload)
    return {name: {'sha256': digest(payload), 'size': len(payload)} for name, payload in fixtures.items()}


def malicious_archives(directory: Path, *, declared_limit: int = 2 << 30):
    """No unsafe path is ever written to disk: only archive headers contain it."""
    directory.mkdir(parents=True, exist_ok=True)
    output = {}
    for kind, name in [('traversal', '../outside-canary'), ('absolute', '/tmp/powerlinux-qa-outside-canary')]:
        path = directory / (kind + '.zip')
        with zipfile.ZipFile(path, 'w', zipfile.ZIP_DEFLATED) as archive:
            archive.writestr('valid-before.txt', b'valid entry must not be partially committed')
            archive.writestr(name, b'escape denied')
        output[kind] = path
    path = directory / 'symlink.zip'
    with zipfile.ZipFile(path, 'w') as archive:
        header = zipfile.ZipInfo('escape-link')
        header.create_system = 3
        header.external_attr = (stat.S_IFLNK | 0o777) << 16
        archive.writestr(header, '../outside-canary')
    output['zip_symlink'] = path
    for kind, entry_type in [('symlink', tarfile.SYMTYPE), ('hardlink', tarfile.LNKTYPE), ('device', tarfile.CHRTYPE), ('fifo', tarfile.FIFOTYPE)]:
        path = directory / (kind + '.tar')
        with tarfile.open(path, 'w') as archive:
            header = tarfile.TarInfo('escape-entry')
            header.type = entry_type
            header.linkname = '../outside-canary'
            header.devmajor, header.devminor = 1, 3
            archive.addfile(header)
        output['tar_' + kind] = path
    path = directory / 'oversized-declaration.tar'
    with tarfile.open(path, 'w') as archive:
        header = tarfile.TarInfo('oversized.bin')
        header.size = declared_limit + 1
        # A tiny intentionally truncated archive must be rejected from its
        # declared size, without allocating or expanding several GiB.
        archive.addfile(header)
    output['oversized'] = path
    return output
