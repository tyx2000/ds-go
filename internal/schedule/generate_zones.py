#!/usr/bin/env python3
"""Regenerate the official Intl canonical-name data, with no runtime Node dependency."""
import json
import pathlib
import re
import subprocess
import zipfile

root = pathlib.Path(subprocess.check_output(["go", "env", "GOROOT"], text=True).strip())
with zipfile.ZipFile(root / "lib/time/zoneinfo.zip") as archive:
    zones = sorted(name for name in archive.namelist() if re.fullmatch(r"[A-Za-z][A-Za-z0-9_+.-]*(?:/[A-Za-z0-9_+.-]+)+", name))
script = r"""
let input='';for await(const chunk of process.stdin)input+=chunk;
const zones={utc:'UTC'};
for(const name of JSON.parse(input)){
  try{
    const canonical=new Intl.DateTimeFormat('en-US',{timeZone:name}).resolvedOptions().timeZone;
    if(canonical==='UTC'||/^[A-Za-z][A-Za-z0-9_+.-]*(?:\/[A-Za-z0-9_+.-]+)+$/.test(canonical))zones[name.toLowerCase()]=canonical;
  }catch{}
}
process.stdout.write(JSON.stringify({generatedFrom:{node:process.version,icu:process.versions.icu,tz:process.versions.tz},zones},null,2)+'\n');
"""
result = subprocess.check_output(["node", "--input-type=module", "-e", script], input=json.dumps(zones), text=True)
target = pathlib.Path(__file__).with_name("zones.json")
target.write_text(result)
print(f"Generated {len(json.loads(result)['zones'])} canonical zone entries")
