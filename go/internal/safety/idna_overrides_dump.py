"""Dump the per-code-point host mappings of the Python reference (input for idna_overrides_gen.go).

modern: idna.uts46_remap(ch, std3_rules=False, transitional=False), as idna.encode(uts46=True)
        applies it (None = disallowed). Code points whose mapping contains a character CPython's
        unicodedata has no bidi class for are listed in "doomed": check_bidi rejects any label
        containing them, so they are an error whatever the mapping.
legacy: for code points assigned in Unicode 3.2, what the stdlib idna codec's nameprep does to
        the character on its own: "" for table B.1, else NFKC(B.2 mapping) over Unicode 3.2.

Run from python/:
    uv run python ../go/internal/safety/idna_overrides_dump.py /tmp/idna_dump.json
then from go/internal/safety:
    go run idna_overrides_gen.go /tmp/idna_dump.json | gofmt > idna_overrides.go
"""

import json
import stringprep
import sys
import unicodedata

import idna

modern, doomed, legacy = {}, [], {}
for c in range(0x110000):
    if 0xD800 <= c < 0xE000:
        continue
    ch = chr(c)
    try:
        out = idna.uts46_remap(ch, std3_rules=False, transitional=False)
    except idna.IDNAError:
        out = None
    if out != ch:
        modern[c] = out
    if out is not None and any(unicodedata.bidirectional(x) == "" for x in out):
        doomed.append(c)
    if unicodedata.ucd_3_2_0.category(ch) != "Cn":
        prep = "" if stringprep.in_table_b1(ch) else unicodedata.ucd_3_2_0.normalize(
            "NFKC", stringprep.map_table_b2(ch))
        if prep != ch:
            legacy[c] = prep
json.dump({"modern": modern, "doomed": doomed, "legacy": legacy,
           "assigned32": [c for c in range(0x110000) if not 0xD800 <= c < 0xE000
                          and unicodedata.ucd_3_2_0.category(chr(c)) != "Cn"],
           "idna": idna.__version__}, open(sys.argv[1], "w"))
