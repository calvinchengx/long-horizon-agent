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
from lha.verify.trusted import (
    CommandTrustedRunner,
    TrustedAwareVerifier,
    TrustedRunner,
    candidate_commit,
)
from lha.verify.verifier import DeterministicVerifier, clip_output_tail, default_python_checks
from lha.verify.witnesses import (
    UnknownTrustedCheck,
    item_checks,
    parse_witness,
    validate_witness,
)

__all__ = [
    "CommandTrustedRunner",
    "DeterministicVerifier",
    "FlakeEvidenceError",
    "FlakyQuarantine",
    "MutationResult",
    "MutationToolError",
    "TrustBootstrap",
    "TrustedAwareVerifier",
    "TrustedRunner",
    "UnknownTrustedCheck",
    "VerifierTrust",
    "candidate_commit",
    "checks_from_commands",
    "clip_output_tail",
    "default_python_checks",
    "derive_check_name",
    "ensure_unique_check_names",
    "harness_violations",
    "item_checks",
    "parse_coverage_pct",
    "parse_total_coverage",
    "parse_witness",
    "run_mutation_testing",
    "snapshot_harness",
    "validate_witness",
]
