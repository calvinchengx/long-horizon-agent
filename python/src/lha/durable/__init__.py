"""The durable control plane (Temporal): the spine that makes long runs survivable.

Kept import-light on purpose — the workflow sandbox re-imports this package, so heavy/
non-deterministic modules (git, model, sandbox) are pulled in only inside activities or under
``workflow.unsafe.imports_passed_through()``.
"""
