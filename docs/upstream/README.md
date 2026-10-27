# Pinned upstream references

These original configuration files come from `deepseek-ai/deepseek-harness` commit `687f282e8625c6b8d11ff58211a90524be2e4317` and are retained under the upstream MIT license in this directory. They are documentation inputs, not dynamically loaded application plugins.

| Local file | Upstream path |
| --- | --- |
| base.cordis.patch.yml | packages/bundle/base/cordis.patch.yml |
| web-app.cordis.patch.yml | packages/bundle/web-app/cordis.patch.yml |
| standard.patch.yml | packages/bundle/web-app/presets/standard.patch.yml |
| minimal.patch.yml | packages/bundle/web-app/presets/minimal.patch.yml |
| ptc.patch.yml | packages/bundle/web-app/presets/ptc.patch.yml |
| cordis.patch.yml | packages/bundle/web-app/presets/cordis.patch.yml |

The source identities and hashes are recorded in `manifest.json`. Run `python3 scripts/verify-upstream.py` from the repository to verify that the recorded inputs have not changed. Source presence is not a parity test; the behavior mapping lives in `docs/PARITY.md`.
