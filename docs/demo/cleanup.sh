#!/usr/bin/env bash
set -euo pipefail

# Portal Demo — Cleanup
#
# Deletes the KIND clusters and generated artifacts from demo.sh.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"

RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
NC='\033[0m'

info()  { echo -e "${BLUE}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }

info "Deleting KIND cluster: portal-source"
kind delete cluster --name portal-source 2>/dev/null || true
ok "Deleted portal-source"

info "Deleting KIND cluster: portal-destination"
kind delete cluster --name portal-destination 2>/dev/null || true
ok "Deleted portal-destination"

info "Removing generated artifacts"
rm -rf "${ROOT_DIR}/demo-tunnel"
rm -f "${ROOT_DIR}/portal"
ok "Cleaned up"

echo ""
ok "All demo resources removed"
