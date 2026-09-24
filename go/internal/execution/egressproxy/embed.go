package egressproxy

import _ "embed"

// PythonSource is the stdlib-only Python proxy (a byte-identical copy of
// python/src/lha/execution/egress_proxy.py; a test keeps them in sync). The Docker sandbox runs
// it in its proxy container as `python -c <source>`, exactly like the Python implementation.
//
//go:embed egress_proxy.py
var PythonSource string
