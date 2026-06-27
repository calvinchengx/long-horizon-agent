"""Seeding a mission from the operator's own checklist (``lha.state.checklist_import``)."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from lha.contracts.state import Checklist, ChecklistItem
from lha.state.checklist_import import (
    ChecklistImportError,
    ImportedChecklist,
    load_checklist,
    witnesses_from_manifest,
)


def _write(tmp_path: Path, name: str, content: str) -> Path:
    path = tmp_path / name
    path.write_text(content, encoding="utf-8")
    return path


def _json(tmp_path: Path, data: object, name: str = "plan.json") -> Path:
    return _write(tmp_path, name, json.dumps(data))


# --- JSON -----------------------------------------------------------------------------------
def test_json_mission_file(tmp_path: Path) -> None:
    path = _json(
        tmp_path,
        {
            "title": " Livy ",
            "description": "Real Spark behind Livy.",
            "references": ["docs/livy.md"],
            "items": [
                {"description": "sessions API", "witnesses": ["go:TestLivySessions"]},
                {"description": "batches API", "depends_on": ["01"], "allow_harness_edits": True},
                {
                    "id": "e2e",
                    "description": "real spark",
                    "depends_on": ["02"],
                    "witnesses": ["ci:sail"],
                },
            ],
        },
    )
    imported = load_checklist(path)
    assert isinstance(imported, ImportedChecklist)
    assert imported.title == "Livy" and imported.description == "Real Spark behind Livy."
    assert imported.references == ["docs/livy.md"]
    items = imported.checklist.items
    assert [i.id for i in items] == ["01", "02", "e2e"]
    assert items[0].witnesses == ["go:TestLivySessions"]
    assert items[1].allow_harness_edits and items[1].depends_on == ["01"]
    assert all(i.status == "todo" for i in items)


def test_json_checklist_dump_round_trips(tmp_path: Path) -> None:
    checklist = Checklist(
        items=[
            ChecklistItem(id="a", description="one", status="done", verified_by=["pytest"]),
            ChecklistItem(id="b", description="two", depends_on=["a"], witnesses=["cmd:true"]),
        ]
    )
    path = _write(tmp_path, "checklist.json", checklist.model_dump_json())
    imported = load_checklist(path)
    assert imported.checklist == checklist
    assert imported.title == "" and imported.references == []


def test_json_top_level_list_and_id_width(tmp_path: Path) -> None:
    path = _json(tmp_path, [{"description": f"step {n}"} for n in range(100)])
    ids = [i.id for i in load_checklist(path).checklist.items]
    assert ids[0] == "001" and ids[-1] == "100"


@pytest.mark.parametrize(
    ("data", "fragment"),
    [
        ({"title": "x"}, 'an object with an "items" list'),
        ("just a string", 'an object with an "items" list'),
        ({"items": []}, "checklist has no items"),
        ({"items": ["bare string"]}, "items[0] must be an object"),
        (
            {"items": [{"description": "x", "witness": ["go:TestX"]}]},
            "unknown field(s): ['witness']",
        ),
        ({"items": [{"description": "x", "status": "finished"}]}, "items[0] is invalid"),
        ({"items": [{"description": "  "}]}, "non-empty id and description"),
        ({"title": 3, "items": [{"description": "x"}]}, "must be strings"),
        ({"references": "docs", "items": [{"description": "x"}]}, '"references" must be a list'),
        (
            {"items": [{"id": "01", "description": "x"}, {"description": "y", "id": "01"}]},
            "duplicate item id '01'",
        ),
        ({"items": [{"description": "x", "depends_on": ["zz"]}]}, "unknown item 'zz'"),
        (
            {
                "items": [
                    {"id": "a", "description": "x", "depends_on": ["b"]},
                    {"id": "b", "description": "y", "depends_on": ["a"]},
                ]
            },
            "dependency cycle",
        ),
        ({"items": [{"description": "x", "witnesses": ["sdk:TestX"]}]}, "item '01': witness"),
    ],
)
def test_json_errors(tmp_path: Path, data: object, fragment: str) -> None:
    path = _json(tmp_path, data)
    with pytest.raises(ChecklistImportError) as info:
        load_checklist(path)
    assert fragment in str(info.value)
    assert str(path) in str(info.value)


def test_invalid_json(tmp_path: Path) -> None:
    with pytest.raises(ChecklistImportError, match="invalid JSON"):
        load_checklist(_write(tmp_path, "bad.json", "{nope"))


def test_unreadable_and_unsupported_files(tmp_path: Path) -> None:
    with pytest.raises(ChecklistImportError, match="cannot read checklist"):
        load_checklist(tmp_path / "missing.json")
    with pytest.raises(ChecklistImportError, match=r"unsupported checklist format '\.yaml'"):
        load_checklist(_write(tmp_path, "plan.yaml", "items: []"))


# --- Markdown -------------------------------------------------------------------------------
ROADMAP = """# 13 — Roadmap

Scope chosen: **full** — control plane through the OneLake data plane,
composed via docker-compose.

Each phase is independently useful and CI-verified.

## P0 — the spine (token acceptance + workspaces)

The minimum that lets someone test automation.

- [x] Token acceptance: validate Bearer against entra-emulator JWKS/issuer;
      audience set. (`--entra-issuer`)
- [x] Store + migrations (`workspace`, `item`).

## P1 — CI/CD (the primary draw)

- [x] Item **definitions**: `getDefinition` (200) / `updateDefinition` (202 LRO).
- [ ] **Deployment pipelines** — the third item in this phase's own header,
      shipped end to end (witnesses: go:TestDeployStage, ci:fabric-cli).
      - D0 pipeline/stage model — a nested bullet, not a checkbox
        with its own continuation
      Designed in 23-deployment-pipelines.md.
- [ ] Jobs: trigger + cancel (witness: go:TestJobLifecycle@./internal/jobs/...)

## P2 — the identity handshake

Its dependency has already shipped.

- [ ] Workspace-identity lifecycle (witness: go:TestProvisionIdentity)
  - [ ] nested checkbox is its own item
- [ ] Trusted workspace access

```
- [ ] not an item: inside a code fence
```

### Notes

- a plain bullet is ignored

## Sequencing note

Prose only, no items.

## R — Real compute

- [ ] Livy on real Spark (witness: pytest:tests/e2e/test_livy.py::test_real_spark)
"""


def test_fabric_style_roadmap(tmp_path: Path) -> None:
    imported = load_checklist(_write(tmp_path, "13-roadmap.md", ROADMAP))
    assert imported.title == "13 — Roadmap"
    assert imported.description == (
        "Scope chosen: **full** — control plane through the OneLake data plane, "
        "composed via docker-compose.\n\nEach phase is independently useful and CI-verified."
    )
    assert imported.references == []
    items = {i.id: i for i in imported.checklist.items}
    assert list(items) == ["01", "02", "03", "04", "05", "06"]

    pipelines = items["01"]
    assert pipelines.description == (
        "**Deployment pipelines** — the third item in this phase's own header, "
        "shipped end to end. Designed in 23-deployment-pipelines.md."
    )
    assert pipelines.witnesses == ["go:TestDeployStage", "ci:fabric-cli"]
    assert items["02"].witnesses == ["go:TestJobLifecycle@./internal/jobs/..."]
    assert items["02"].description == "Jobs: trigger + cancel"
    # P0 has only done items, so the first open phase has no dependencies.
    assert pipelines.depends_on == [] and items["02"].depends_on == []
    # P2 depends on every item of P1; items inside P2 are independent.
    for item_id in ("03", "04", "05"):
        assert items[item_id].depends_on == ["01", "02"]
    assert items["04"].description == "nested checkbox is its own item"
    # R skips the item-less "Sequencing note" phase and depends on all of P2.
    assert items["06"].depends_on == ["03", "04", "05"]
    assert items["06"].witnesses == ["pytest:tests/e2e/test_livy.py::test_real_spark"]
    assert all("not an item" not in i.description for i in items.values())


def test_include_done_imports_checked_items_as_todo(tmp_path: Path) -> None:
    imported = load_checklist(_write(tmp_path, "roadmap.md", ROADMAP), include_done=True)
    items = imported.checklist.items
    assert len(items) == 9
    assert items[0].description == (
        "Token acceptance: validate Bearer against entra-emulator JWKS/issuer; "
        "audience set. (`--entra-issuer`)"
    )
    assert all(i.status == "todo" for i in items)
    p0 = ["01", "02"]
    assert items[2].depends_on == p0  # P1's done item depends on P0
    assert items[3].depends_on == p0


def test_markdown_without_title_uses_file_stem(tmp_path: Path) -> None:
    imported = load_checklist(_write(tmp_path, "plan.md", "- [ ] one\n- [ ] two\n"))
    assert imported.title == "plan" and imported.description == ""
    assert [i.depends_on for i in imported.checklist.items] == [[], []]


def test_markdown_items_before_first_phase_are_a_phase(tmp_path: Path) -> None:
    text = "# T\n\n- [ ] setup\n\n## Next\n\n- [ ] build\n"
    items = load_checklist(_write(tmp_path, "p.md", text)).checklist.items
    assert [i.depends_on for i in items] == [[], ["01"]]


def test_markdown_multiple_witness_groups_and_case(tmp_path: Path) -> None:
    text = "- [ ] thing (Witness: go:TestA) and (WITNESSES: cmd:make x, trusted:e2e,)\n"
    item = load_checklist(_write(tmp_path, "p.md", text)).checklist.items[0]
    assert item.witnesses == ["go:TestA", "cmd:make x", "trusted:e2e"]
    assert item.description == "thing and"


def test_markdown_many_items_get_wide_ids(tmp_path: Path) -> None:
    text = "".join(f"- [ ] item {n}\n" for n in range(120))
    items = load_checklist(_write(tmp_path, "p.md", text)).checklist.items
    assert items[0].id == "001" and items[-1].id == "120"


def test_markdown_errors_carry_line_numbers(tmp_path: Path) -> None:
    path = _write(tmp_path, "p.md", "# T\n\n- [ ] fine\n- [ ] bad (witness: go:not-a-test)\n")
    with pytest.raises(ChecklistImportError) as info:
        load_checklist(path)
    message = str(info.value)
    assert message.startswith(f"{path}:4: item '02': witness 'go:not-a-test'")


def test_markdown_item_with_only_a_witness(tmp_path: Path) -> None:
    path = _write(tmp_path, "p.md", "## P\n\n- [ ] (witness: go:TestX)\n")
    with pytest.raises(ChecklistImportError, match=r"p\.md:3: checklist item has no description"):
        load_checklist(path)


def test_markdown_without_open_items(tmp_path: Path) -> None:
    path = _write(tmp_path, "p.md", "# T\n\n- [x] already done\n- plain bullet\n")
    with pytest.raises(ChecklistImportError, match="checklist has no items"):
        load_checklist(path)


# --- witnesses.json manifests ---------------------------------------------------------------
def test_witnesses_from_manifest(tmp_path: Path) -> None:
    path = _json(
        tmp_path,
        {
            "row-level-security": {
                "section": "Data Warehouse",
                "claim": "RLS",
                "witnesses": ["ci:warehouse-tds", "go:TestRelayEnforcesRowLevelSecurity"],
            },
            "blob-surface": {"witnesses": ["ci:adls-sdk"]},
            "_gated": {"go:TestRelayEnforcesRowLevelSecurity": "needs SQL Server"},
        },
        name="witnesses.json",
    )
    assert witnesses_from_manifest(path) == {
        "row-level-security": ["ci:warehouse-tds", "go:TestRelayEnforcesRowLevelSecurity"],
        "blob-surface": ["ci:adls-sdk"],
    }


@pytest.mark.parametrize(
    ("content", "fragment"),
    [
        ("{nope", "cannot read witness manifest"),
        ("[1, 2]", "must be a JSON object"),
        ('{"a": {"claim": "x"}}', "'a'.witnesses must be a list of strings"),
        ('{"a": ["go:TestX"]}', "'a'.witnesses must be a list of strings"),
        ('{"a": {"witnesses": [1]}}', "'a'.witnesses must be a list of strings"),
    ],
)
def test_witness_manifest_errors(tmp_path: Path, content: str, fragment: str) -> None:
    with pytest.raises(ChecklistImportError) as info:
        witnesses_from_manifest(_write(tmp_path, "w.json", content))
    assert fragment in str(info.value)


def test_witness_manifest_missing_file(tmp_path: Path) -> None:
    with pytest.raises(ChecklistImportError, match="cannot read witness manifest"):
        witnesses_from_manifest(tmp_path / "absent.json")
