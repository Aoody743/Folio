#!/usr/bin/env python3
"""Ship the Astryx field separator missing from Inter and Noto SC (SIL OFL)."""
from pathlib import Path
from fontTools import subset
from fontTools.ttLib import TTFont

root = Path(__file__).resolve().parents[2]
font = TTFont(root / 'frontend/public/fonts/source-sans.woff2', recalcTimestamp=False)
options = subset.Options()
options.recalc_timestamp = False
worker = subset.Subsetter(options=options)
worker.populate(unicodes=[0x2219])
worker.subset(font)
assert font.getBestCmap().get(0x2219) not in (None, '.notdef')
font.flavor = 'woff2'
path = root / 'frontend/public/fonts/folio-symbols.woff2'
font.save(path)
path.chmod(0o644)
print(f'{path.name}: {path.stat().st_size} bytes, U+2219')
