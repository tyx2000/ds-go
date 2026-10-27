#!/usr/bin/env python3
"""Check pinned migration inputs without downloading or executing upstream code."""
import hashlib
import json
from pathlib import Path

root = Path(__file__).resolve().parents[1]
manifest = json.loads((root / "docs/upstream/manifest.json").read_text())
for entry in manifest["files"]:
    data = (root / "docs/upstream" / entry["local"]).read_bytes()
    digest = hashlib.sha256(data).hexdigest()
    blob = hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest()
    if digest != entry["sha256"] or blob != entry["git_blob"]:
        raise SystemExit("Upstream reference changed: " + entry["local"])
print("Verified %d pinned upstream inputs at %s" % (len(manifest["files"]), manifest["commit"]))
