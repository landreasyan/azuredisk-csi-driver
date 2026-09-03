#!/bin/bash
#
# doccut.sh - Automates the "doc: cut vX.Y.Z release" PR changes for a release
# branch of kubernetes-sigs/azuredisk-csi-driver (e.g. release-1.33, release-1.34).
#
# This reproduces the same file changes as prior "doc: cut" PRs (e.g. #3744, #3745):
#   - bump IMAGE_VERSION in Makefile and the version table in README.md
#   - bump charts/latest/azuredisk-csi-driver Chart.yaml/values.yaml and repackage
#   - add a new charts/vX.Y.Z chart snapshot
#   - bump image tags in top-level deploy/*.yaml manifests
#   - add a new deploy/vX.Y.Z manifest snapshot
#   - regenerate charts/index.yaml (and dedupe the entry helm creates for the
#     "latest" tgz vs the versioned tgz, keeping only the versioned one - matches
#     the convention already present in charts/index.yaml)
#   - add docs/install-csi-driver-vX.Y.Z.md and update docs/install-azuredisk-csi-driver.md
#
# Requirements: helm (v3) and python3 with pyyaml available on PATH.
#
# Usage: doccut.sh <worktree_dir> <branch> <old_version> <new_version>
#   worktree_dir  - path to a clean git worktree/checkout of <branch>
#   branch        - release branch name, e.g. release-1.33 (used in charts/index.yaml URLs)
#   old_version   - current version without leading "v", e.g. 1.33.11
#   new_version   - new version without leading "v", e.g. 1.33.12
#
# Example:
#   git worktree add /tmp/doccut-1.33 origin/release-1.33
#   ./doccut.sh /tmp/doccut-1.33 release-1.33 1.33.11 1.33.12
#   cd /tmp/doccut-1.33 && git add -A && git commit -m "doc: cut v1.33.12 release"
#
# After running, it's recommended to sanity-check with:
#   helm lint charts/latest/azuredisk-csi-driver
#   (re-extract each charts/*/*.tgz and confirm no diff vs the source templates,
#    same check as hack/verify-helm-chart-files.sh performs)

set -euo pipefail

WT="$1"
BRANCH="$2"
OLD="$3"
NEW="$4"

cd "$WT"

echo "=== Creating branch cut-release-v${NEW} ==="
git checkout -b "cut-release-v${NEW}"

echo "=== Updating Makefile ==="
sed -i "s/IMAGE_VERSION ?= v${OLD}/IMAGE_VERSION ?= v${NEW}/" Makefile

echo "=== Updating README.md ==="
sed -i "s#|v${OLD}         |mcr.microsoft.com/oss/v2/kubernetes-csi/azuredisk-csi:v${OLD}#|v${NEW}         |mcr.microsoft.com/oss/v2/kubernetes-csi/azuredisk-csi:v${NEW}#" README.md

echo "=== Updating charts/latest ==="
sed -i "s/appVersion: ${OLD}/appVersion: ${NEW}/;s/^version: ${OLD}/version: ${NEW}/" charts/latest/azuredisk-csi-driver/Chart.yaml
sed -i "s/tag: v${OLD}/tag: v${NEW}/" charts/latest/azuredisk-csi-driver/values.yaml

echo "=== Creating charts/v${NEW} snapshot ==="
mkdir -p "charts/v${NEW}"
cp -r charts/latest/azuredisk-csi-driver "charts/v${NEW}/azuredisk-csi-driver"

echo "=== Packaging helm charts ==="
rm -f charts/latest/azuredisk-csi-driver-*.tgz
helm package charts/latest/azuredisk-csi-driver -d charts/latest/ >/dev/null
helm package "charts/v${NEW}/azuredisk-csi-driver" -d "charts/v${NEW}/" >/dev/null

echo "=== Updating deploy/*.yaml top-level manifests ==="
for f in deploy/csi-azuredisk-controller.yaml deploy/csi-azuredisk-node.yaml deploy/csi-azuredisk-node-windows.yaml; do
  sed -i "s#azuredisk-csi:v${OLD}#azuredisk-csi:v${NEW}#" "$f"
done
sed -i "s#azuredisk-csi:v${OLD}-windows-hp#azuredisk-csi:v${NEW}-windows-hp#g" deploy/csi-azuredisk-node-windows-hostprocess.yaml

echo "=== Creating deploy/v${NEW} snapshot ==="
mkdir -p "deploy/v${NEW}"
for f in crd-csi-snapshot.yaml csi-azuredisk-controller.yaml csi-azuredisk-driver.yaml \
         csi-azuredisk-node-windows-hostprocess.yaml csi-azuredisk-node-windows.yaml csi-azuredisk-node.yaml \
         csi-snapshot-controller.yaml rbac-csi-azuredisk-controller.yaml rbac-csi-azuredisk-node.yaml \
         rbac-csi-snapshot-controller.yaml; do
  cp "deploy/$f" "deploy/v${NEW}/$f"
done

echo "=== Regenerating charts/index.yaml ==="
helm repo index charts --url "https://raw.githubusercontent.com/kubernetes-sigs/azuredisk-csi-driver/${BRANCH}/charts" >/dev/null

echo "=== Deduplicating charts/index.yaml (helm indexes both the 'latest' and versioned tgz) ==="
python3 - "$NEW" <<'PYEOF'
import sys
import yaml

new_version = sys.argv[1]
path = "charts/index.yaml"
with open(path) as f:
    data = yaml.safe_load(f)

entries = data['entries']['azuredisk-csi-driver']
kept = []
for e in entries:
    if e['version'] == new_version and '/latest/' in e['urls'][0]:
        continue  # drop the duplicate that points at the "latest" tgz
    kept.append(e)
data['entries']['azuredisk-csi-driver'] = kept

with open(path, 'w') as f:
    yaml.safe_dump(data, f, default_flow_style=False, sort_keys=False)
PYEOF

# yaml.safe_dump quotes timestamps with single quotes; the repo convention uses
# double quotes, so normalize to keep the diff minimal.
python3 - <<'PYEOF'
import re
path = "charts/index.yaml"
with open(path) as f:
    content = f.read()
content = re.sub(r"'((?:[0-9T:.Z-]+))'", r'"\1"', content)
with open(path, 'w') as f:
    f.write(content)
PYEOF

echo "=== Updating docs ==="
sed -i "s#\[install v${OLD} CSI driver\](./install-csi-driver-v${OLD}.md)#[install v${NEW} CSI driver](./install-csi-driver-v${NEW}.md)#" docs/install-azuredisk-csi-driver.md
sed "s/${OLD}/${NEW}/g" "docs/install-csi-driver-v${OLD}.md" > "docs/install-csi-driver-v${NEW}.md"

echo "=== Done applying doc-cut changes for v${NEW} on ${BRANCH} ==="
