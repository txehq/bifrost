#!/usr/bin/env python3
"""Every client setting in transports/config.schema.json has a Helm chart value.

The chart builds config.json's `client` section key by key in templates/_helpers.tpl, so a
client setting added to the gateway without a chart mapping cannot be set through Helm. A
deployment then sets it through PUT /api/config, and a later `helm upgrade` that changes the
client section applies the file again and silently puts the setting back to its default
(TXE-7038: the two TXE-6519 switches shipped without chart values).

For each property of `client` in the config schema, this check requires one of:
  - a mapping in _helpers.tpl (`set $client "<key>" ...`) whose `.Values.bifrost.client.<name>`
    is declared in the chart's values.schema.json, or
  - an entry in NOT_IN_CHART below, with the reason.

It also fails on a NOT_IN_CHART entry that is mapped or no longer in the schema, and on a
mapped key the schema does not have, so the list cannot go stale.

Usage: check-helm-client-config-keys.py [--schema F] [--helpers F] [--values-schema F]
"""

import argparse
import json
import re
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]

# Client settings the chart does not render yet. Each was already missing when this check was
# added (TXE-7038); TXE-7060 tracks giving them chart values. Do not add a new setting here to
# get a pull request through: map it in _helpers.tpl instead.
NOT_IN_CHART = {
    "a2a_store_raw_request_response": "not mapped before TXE-7038; TXE-7060",
    "enforce_scim_auth": "not mapped before TXE-7038; TXE-7060",
    "enforce_vk_object_ownership": "not mapped before TXE-7038; TXE-7060",
    "mcp_code_mode_limits": "not mapped before TXE-7038; TXE-7060",
    "mcp_oauth_private_allowlist": "not mapped before TXE-7038; TXE-7060",
}

SET_CLIENT = re.compile(r'set \$client "([a-z0-9_]+)"([^\n]*)')
VALUES_CLIENT = re.compile(r"\.Values\.bifrost\.client\.([A-Za-z0-9]+)")


def resolve(schema, node):
    while "$ref" in node:
        target = schema
        for part in node["$ref"].lstrip("#/").split("/"):
            target = target[part]
        node = target
    return node


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--schema", default=REPO_ROOT / "transports/config.schema.json")
    parser.add_argument("--helpers", default=REPO_ROOT / "helm-charts/bifrost/templates/_helpers.tpl")
    parser.add_argument("--values-schema", default=REPO_ROOT / "helm-charts/bifrost/values.schema.json")
    args = parser.parse_args()

    schema = json.loads(Path(args.schema).read_text())
    schema_keys = set(resolve(schema, schema["properties"]["client"])["properties"])

    values_schema = json.loads(Path(args.values_schema).read_text())
    chart_client = resolve(values_schema, values_schema["properties"]["bifrost"])
    chart_values = set(resolve(values_schema, chart_client["properties"]["client"])["properties"])

    mapped = {}
    for key, rest in SET_CLIENT.findall(Path(args.helpers).read_text()):
        mapped.setdefault(key, set()).update(VALUES_CLIENT.findall(rest))

    errors = []
    for key in sorted(schema_keys - set(mapped) - set(NOT_IN_CHART)):
        errors.append(
            f"client.{key} is in config.schema.json but _helpers.tpl never sets it: "
            "add a bifrost.client value and its mapping, so a Helm deployment can set it"
        )
    for key in sorted(set(mapped) - schema_keys):
        errors.append(f"_helpers.tpl sets client.{key}, which config.schema.json does not have")
    for key, names in sorted(mapped.items()):
        for name in sorted(names - chart_values):
            errors.append(
                f"client.{key} is rendered from bifrost.client.{name}, which values.schema.json does not declare"
            )
    for key in sorted(NOT_IN_CHART):
        if key in mapped:
            errors.append(f"client.{key} is mapped in _helpers.tpl: remove it from NOT_IN_CHART")
        elif key not in schema_keys:
            errors.append(f"client.{key} is no longer in config.schema.json: remove it from NOT_IN_CHART")

    if errors:
        print("❌ Helm chart client values are out of step with config.schema.json:", file=sys.stderr)
        for error in errors:
            print(f"  - {error}", file=sys.stderr)
        return 1
    print(
        f"✅ {len(schema_keys)} client settings: {len(schema_keys) - len(NOT_IN_CHART)} have a chart value, "
        f"{len(NOT_IN_CHART)} are listed as not in the chart"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
