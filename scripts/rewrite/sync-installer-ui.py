#!/usr/bin/env python3
"""Embed reviewed installer presentation code; release archives remain self-contained."""
import argparse
import json
from pathlib import Path
import re
import shlex

root = Path(__file__).resolve().parent
parser = argparse.ArgumentParser()
parser.add_argument('--check', action='store_true')
args = parser.parse_args()
messages = json.loads((root / 'installer-messages.zh-CN.json').read_text())
for source, translated in messages.items():
    if re.findall(r'%(?:s|d)', source) != re.findall(r'%(?:s|d)', translated):
        raise SystemExit(f'Format placeholders differ: {source!r}')
cases = '\n'.join(f'      {shlex.quote(key)}) ui_key={shlex.quote(value)} ;;' for key, value in sorted(messages.items()))
ui = (root / 'installer-ui.sh.in').read_text().replace('__TRANSLATIONS__', cases).rstrip()
for path in (root / 'install.sh', root / 'install-1.0-beta4.sh'):
    source = path.read_text()
    if '# BEGIN GENERATED INSTALLER UI' in source:
        updated = re.sub(r'# BEGIN GENERATED INSTALLER UI.*?# END GENERATED INSTALLER UI', lambda _: ui, source, flags=re.S)
    else:
        updated = source.replace('umask 077\n', 'umask 077\n\n' + ui + '\n', 1)
    if args.check:
        if updated != source:
            raise SystemExit(f'{path.name}: run sync-installer-ui.py')
    else:
        path.write_text(updated)
