"""The System One command second-opinion question: is a command the classifier ALLOWED dangerous?

The deterministic command classifier (``lha.safety.commands``) is the boundary: it gates
irreversible or outward-facing commands. This question is asked only about a ``run_command`` the
classifier ALLOWED, so a model can add a gate the rules did not know to add. The answer can only
ever make LHA more cautious: a confident ``dangerous`` sends the call to the human gate exactly as
an irreversible command is (denied when there is no gate); anything else, or a failed call,
changes nothing. It never allows a command the classifier denied and never changes the classifier.

The question is worded after TypeSafe's guidance on Jev's weaknesses: neutral option names with
full descriptions, one judgment per question, and only the text the judgment needs. The state is
the command argv alone -- never tool output.
"""

from __future__ import annotations

from pydantic import JsonValue

from lha.contracts.system_one import Choice
from lha.obs.redact import redact_text

QUESTION_ID = "danger"

CRITERIA: dict[str, str] = {
    "routine": (
        "The command is an ordinary, reversible action with no data leaving the machine and no "
        "risk to the host."
    ),
    "dangerous": (
        "The command could damage the host, delete or overwrite data, exfiltrate data over the "
        "network, or is irreversible or outward-facing."
    ),
}

QUESTION = Choice(
    instructions=(
        "The command `command` is about to run. Is it an ordinary, reversible action, or one a "
        "human should decide on first?"
    ),
    criteria=dict(CRITERIA),
)


def command_state(command: list[str]) -> dict[str, JsonValue]:
    """What the model sees: only the command (redacted), never tool output."""
    return {"command": redact_text(" ".join(command))}
