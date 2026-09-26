# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
import sys

nb = json.load(open(sys.argv[1], encoding="utf-8"))
for i, cell in enumerate(nb["cells"]):
    outs = cell.get("outputs", [])
    print(f"cell {i} [{cell['cell_type']}] outputs={len(outs)}")
print("---- code cell outputs ----")
for cell in nb["cells"]:
    if cell["cell_type"] != "code":
        continue
    for out in cell.get("outputs", []):
        text = out.get("text") or out.get("data", {}).get("text/html") or ""
        print(text[:500])
