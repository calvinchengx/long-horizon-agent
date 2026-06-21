"""The verification plane: the deterministic gate that decides what "done" means."""

from lha.contracts.verify import checks_from_commands, derive_check_name, ensure_unique_check_names
from lha.verify.flaky_quarantine import FlakeEvidenceError, FlakyQuarantine
from lha.verify.harness_integrity import harness_violations, snapshot_harness
from lha.verify.mutation import MutationResult, MutationToolError, run_mutation_testing
from lha.verify.trust_bootstrap import (
    TrustBootstrap,
    VerifierTrust,
    parse_coverage_pct,
    parse_total_coverage,
)
from lha.verify.verifier import DeterministicVerifier, clip_output_tail, default_python_checks

__all__ = [
    "DeterministicVerifier",
    "FlakeEvidenceError",
    "FlakyQuarantine",
    "MutationResult",
    "MutationToolError",
    "TrustBootstrap",
    "VerifierTrust",
    "checks_from_commands",
    "clip_output_tail",
    "default_python_checks",
    "derive_check_name",
    "ensure_unique_check_names",
    "harness_violations",
    "parse_coverage_pct",
    "parse_total_coverage",
    "run_mutation_testing",
    "snapshot_harness",
]
