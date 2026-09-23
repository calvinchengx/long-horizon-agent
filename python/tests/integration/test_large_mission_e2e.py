"""One mission that needs every large-mission capability at once, on real Docker.

A miniature of building fabric-emulator: a Go project, driven from a Markdown roadmap.

1. **Sandbox image + egress**: the lead works in the polyglot image (Go + uv + pnpm) and can reach
   only the Go module proxy; ``go mod tidy`` must download golang.org/x/text through it.
2. **Witnesses**: the greet item is done only when ``go:TestHello`` exists and passes.
3. **Trusted runner**: the CLI item's witness is an operator ``trusted:e2e`` check that needs a
   Docker daemon, so it runs outside the sandbox, against the candidate commit.
4. **Replanning**: the first item is too coarse; it blocks and is split into two children.
5. **References**: a vendored reference file is recited to the lead every cycle.
6. **Approval**: publishing needs ``git push``; it is routed to a (simulated) human and allowed.
7. **Checklist import**: the mission starts from the roadmap, not from the Planner.

The model is scripted (one model call per cycle, plus the replanner's), so the test is
deterministic; everything else is real: Docker, the proxy, the verifier, git and the checks.
Opt in with ``LHA_IT_DOCKER=1``; needs the image from ``sandbox/Dockerfile``
(``LHA_IT_SANDBOX_IMAGE``, default ``lha-sandbox:dev``), a local ``go`` and network access.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
from pathlib import Path

import pytest

from lha.agent.runner import run_mission_local
from lha.config import Settings
from lha.contracts.hitl import GateDecision, GateRequest
from lha.contracts.model import ToolCall, TurnResult
from lha.contracts.verify import checks_from_commands
from lha.hitl.gate import CallbackGate
from lha.model.stub import StubModel
from lha.state import git_ops
from lha.state.checklist_import import load_checklist
from tests.integration.conftest import requires_docker

pytestmark = [pytest.mark.integration, requires_docker]
pytest.importorskip("docker")

IMAGE = os.environ.get("LHA_IT_SANDBOX_IMAGE", "lha-sandbox:dev")

ROADMAP = """# Greeter

A tiny greeting service, built the way a large emulator is: phase by phase, each item proven by
its own witness.

## P0

- [ ] Add package greet with func Hello() string returning "hello", and a TestHello test (witness: go:TestHello@./greet/...)

## P1

- [ ] main.go prints greet.Hello() upper-cased with golang.org/x/text/cases (witness: trusted:e2e)
- [ ] Publish the work to origin (witness: cmd:test -f PUBLISHED)
"""

GREET = 'package greet\n\n// Hello returns the greeting.\nfunc Hello() string { return "hello" }\n'
GREET_TEST = (
    'package greet\n\nimport "testing"\n\nfunc TestHello(t *testing.T) {\n'
    '\tif Hello() != "hello" {\n\t\tt.Fatal(Hello())\n\t}\n}\n'
)
MAIN = (
    'package main\n\nimport (\n\t"fmt"\n\n\t"demo/greet"\n\t"golang.org/x/text/cases"\n'
    '\t"golang.org/x/text/language"\n)\n\nfunc main() {\n'
    "\tfmt.Println(cases.Upper(language.English).String(greet.Hello()))\n}\n"
)


def _tool(name: str, **arguments: object) -> TurnResult:
    return TurnResult(
        tool_calls=[ToolCall(id="", name=name, arguments=arguments)], stop_reason="tool_use"
    )


DONE = TurnResult(text='{"done": true, "summary": "done"}', stop_reason="end_turn")
PUBLISH = ["sh", "-c", "git push --dry-run origin HEAD && touch PUBLISHED"]
SCRIPT = [
    # c1-c3: the coarse item "passes" on the model's word only -> the witness fails 3x -> blocked
    DONE,
    DONE,
    DONE,
    # the replanner splits it into two children (same model, metered)
    TurnResult(
        text=json.dumps(
            [
                {"description": "Create greet/greet.go with func Hello() returning hello"},
                {"description": "Add greet/greet_test.go with TestHello"},
            ]
        )
    ),
    _tool("write_file", path="greet/greet.go", content=GREET),  # c4: 01.1
    _tool("write_file", path="greet/greet_test.go", content=GREET_TEST),  # c5: 01.2 + witness
    _tool("write_file", path="main.go", content=MAIN),  # c6: 02 fails: module not downloaded
    # c7: the failed attempt was rolled back, so write main.go again and fetch the module
    # through the egress proxy in one step.
    _tool("run_command", argv=["sh", "-c", f"cat > main.go <<'GO'\n{MAIN}GO\ngo mod tidy"]),
    _tool("run_command", argv=PUBLISH),  # c8: 03, gated -> approved -> runs
]


def _git(cwd: Path, *args: str) -> None:
    subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True)


@pytest.fixture
def workspace(tmp_path: Path) -> Path:
    if shutil.which("go") is None:
        pytest.skip("needs a local go toolchain for the trusted e2e check")
    probe = subprocess.run(["docker", "image", "inspect", IMAGE], capture_output=True)
    if probe.returncode != 0:
        pytest.skip(f"build the sandbox image first: docker build -t {IMAGE} sandbox")
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / "go.mod").write_text("module demo\n\ngo 1.26\n")
    (ws / "Makefile").write_text("e2e:\n\tdocker version >/dev/null && go run . | grep -qx HELLO\n")
    (ws / ".gitignore").write_text(".remote.git/\nPUBLISHED\n")
    (ws / "reference").mkdir()
    (ws / "reference" / "x-text-cases.md").write_text(
        "cases.Upper(language.English).String(s) upper-cases s.\n"
    )
    git_ops.init_repo(ws)
    _git(ws, "init", "--bare", "-q", ".remote.git")
    _git(ws, "remote", "add", "origin", "./.remote.git")
    (tmp_path / "roadmap.md").write_text(ROADMAP)
    return ws


@pytest.mark.asyncio
async def test_a_large_mission_uses_every_capability(workspace: Path, tmp_path: Path) -> None:
    asked: list[GateRequest] = []

    async def human(req: GateRequest) -> GateDecision:
        asked.append(req)
        return GateDecision.APPROVE

    settings = Settings(
        _env_file=None,  # type: ignore[call-arg]
        model_backend="stub",
        sandbox="docker",
        sandbox_image=IMAGE,
        sandbox_egress="proxy.golang.org,sum.golang.org",
        trusted_checks=json.dumps({"e2e": ["make", "e2e"]}),
        harness_paths="Makefile",
        max_turns_per_cycle=1,
        max_replans=2,
        budget_usd_ceiling=1.0,
    )
    imported = load_checklist(tmp_path / "roadmap.md")
    summary = await run_mission_local(
        workdir=str(workspace),
        title=imported.title,
        description=imported.description,
        checklist=imported.checklist,
        checks=checks_from_commands([["go", "build", "./..."], ["go", "vet", "./..."]]),
        settings=settings,
        model=StubModel(script=SCRIPT),
        gate=CallbackGate(human),
        references=["reference/x-text-cases.md"],
    )

    checklist = json.loads((workspace / ".lha" / "checklist.json").read_text())
    failures = {i["id"]: i["last_failure"][-1500:] for i in checklist["items"] if i["last_failure"]}
    assert summary.completed, f"{summary.stopped_reason}\n{json.dumps(failures, indent=1)}"
    status = {item["id"]: item["status"] for item in checklist["items"]}
    assert status == {"01": "split", "01.1": "done", "01.2": "done", "02": "done", "03": "done"}
    by_id = {item["id"]: item for item in checklist["items"]}
    assert "go:TestHello@./greet/..." in by_id["01.2"]["verified_by"]  # witness moved + proven
    assert "trusted:e2e" in by_id["02"]["verified_by"]  # ran outside the sandbox, with Docker
    assert by_id["02"]["attempts"] == 2  # failed until go mod tidy went through the proxy
    # Failed attempts never reach the branch: c6's main.go was rolled back and kept on a ref.
    attempts = git_ops.run_git(
        workspace, "for-each-ref", "--format=%(refname)", "refs/lha/attempts"
    )
    c6 = next(ref for ref in attempts.splitlines() if ref.endswith("/c6"))
    assert "cases.Upper" in git_ops.run_git(workspace, "show", f"{c6}:main.go")
    assert len(asked) == 1 and "git push" in asked[0].context["reason"]  # the approval
    assert (workspace / "PUBLISHED").exists()
    mission = json.loads((workspace / ".lha" / "mission.json").read_text())
    assert mission["references"] == ["reference/x-text-cases.md"]
    log = git_ops.log_oneline(workspace, 50)
    assert any("lha: split 01" in line for line in log)
    assert "golang.org/x/text" in (workspace / "go.mod").read_text()
