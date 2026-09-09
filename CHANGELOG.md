# Codespur Changelog

## 1.0.5 - 2026-09-09
- Added: `--status` flag to show backend config and check connectivity

## 1.0.4 - 2026-09-09
- Added: `--issue-file` flag to pass issue/ticket text as review context
- Added: cross-file consistency pass catching breakage only visible across files (renamed symbols, stale tests)
- Changed: per-file reviews now know which other files changed in the same PR, instead of reviewing in isolation

## 1.0.3 - 2026-07-17
- Added: `-f` / `--diff-file` now accepts a direct raw diff URL

## 1.0.2 - 2026-07-16
- Added: `--diff-file` / `-f` flag to review a saved git diff file

## 1.0.1 - 2026-07-14
- Changed: require `CODESPUR_BASE_URL` and `CODESPUR_MODEL`; drop defaults, exit with error when unset
- Changed: emit `SEVERITY:` line only when an issue is reported; skip it for clean reviews

## 1.0.0 - 2026-07-14
- Initial release
