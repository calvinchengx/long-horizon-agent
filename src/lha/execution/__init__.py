"""The execution plane: sandboxes + the tool dispatcher (where the agent's actions happen)."""

from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.factory import UnsafeSandboxError, build_sandbox, open_sandbox
from lha.execution.sandbox_local import LocalSandbox

# DockerSandbox is intentionally NOT imported here (it requires the `docker` extra); obtain it via
# build_sandbox("docker") or import it from lha.execution.sandbox_docker.

__all__ = [
    "AllowListDispatcher",
    "LocalSandbox",
    "UnsafeSandboxError",
    "build_sandbox",
    "open_sandbox",
]
