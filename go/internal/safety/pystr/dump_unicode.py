"""Dump CPython's str/unicodedata behaviour for the Go pystr tables (see gen.go).

Run with the Python the reference implementation uses:
    cd python && uv run python ../go/internal/safety/pystr/dump_unicode.py /tmp/py_unicode.json
then from go/internal/safety/pystr:  go run gen.go /tmp/py_unicode.json | gofmt > tables.go
"""
import json, sys, unicodedata
cps = [c for c in range(0x110000) if not 0xD800 <= c < 0xE000]
def ranges(pred):
    out=[]; start=None; prev=None
    for c in cps:
        if pred(chr(c)):
            if start is None: start=c
            elif c != prev+1:
                out.append([start,prev]); start=c
            prev=c
    if start is not None: out.append([start,prev])
    return out
# final sigma classes
def X(c): return ("AΣ"+c).lower()[1] == "σ"   # non-ign & cased
def Y(c): return ("A"+c+"Σ").lower()[-1] == "ς"  # ign or cased
data = {
 "space": ranges(str.isspace),
 "alnum": ranges(str.isalnum),
 "digit": ranges(str.isdigit),
 "decimal": ranges(str.isdecimal),
 "cased_nonign": ranges(X),
 "ignorable": ranges(lambda c: Y(c) and not X(c)),
 "nfkc_danger": ranges(lambda c: c not in "/?#@:" and any(x in unicodedata.normalize("NFKC", c) for x in "/?#@:")),
 "lower": {c: chr(c).lower() for c in cps if chr(c).lower()!=chr(c)},
 "upper": {c: chr(c).upper() for c in cps if chr(c).upper()!=chr(c)},
 "casefold": {c: chr(c).casefold() for c in cps if chr(c).casefold()!=chr(c)},
 "unicode": unicodedata.unidata_version,
}
json.dump(data, open(sys.argv[1], "w"))
print({k: len(v) for k,v in data.items()})
