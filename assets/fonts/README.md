# Chinese variable font sources

Original Noto Sans SC and Noto Serif SC fonts downloaded from the official
[google/fonts](https://github.com/google/fonts) repository on 2026-10-22 and
losslessly converted to WOFF2. These sources are outside the public web root.

SIL Open Font License 1.1 notices accompany the shipped fonts:
[Noto Sans SC](../../frontend/public/fonts/noto-sans-sc-OFL.txt),
[Noto Serif SC](../../frontend/public/fonts/noto-serif-sc-OFL.txt).

Regenerate the committed offline subsets with Python and `fonttools[woff]`:

```sh
python3 scripts/rewrite/subset-fonts.py
```

The generator uses the application source to choose a common glyph subset,
partitions remaining characters into disjoint ranges, preserves variable weights,
and asserts complete original cmap coverage. Additional resource text loads only
the required range. Application builds use the generated files and do not need
Python, fontTools or network access.
