#!/usr/bin/env bash
#
# gen-payloads.sh - Generate the payload files that cannot be committed safely:
#   multipart.bin    - needs strict CRLF line endings; git/editors mangle those.
#   json-100kb.json  - a ~100 KB blob is not worth keeping in version control.
#
# Deterministic: same bytes on every run. setup.sh calls this before building
# the payloads ConfigMap; run it by hand if you edit a profile's body.
#
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
OUT="payloads"
mkdir -p "${OUT}"

# --- multipart/form-data (CRLF, boundary must match multipart.env) ----------
BOUNDARY="----perfboundary"
{
  printf -- '--%s\r\n' "${BOUNDARY}"
  printf 'Content-Disposition: form-data; name="title"\r\n\r\n'
  printf 'quarterly report\r\n'
  printf -- '--%s\r\n' "${BOUNDARY}"
  printf 'Content-Disposition: form-data; name="category"\r\n\r\n'
  printf 'finance\r\n'
  printf -- '--%s\r\n' "${BOUNDARY}"
  printf 'Content-Disposition: form-data; name="file"; filename="report.txt"\r\n'
  printf 'Content-Type: text/plain\r\n\r\n'
  printf 'Revenue up 12%% QoQ. Margin steady at 41%%. Headcount 212.\r\n'
  printf -- '--%s--\r\n' "${BOUNDARY}"
} > "${OUT}/multipart.bin"

# --- ~100 KB application/json ----------------------------------------------
python3 - "${OUT}/json-100kb.json" <<'PY'
import json, sys
# Deterministic records; no randomness so the file is byte-stable across runs.
items = [{
    "sku": f"SKU-{i:05d}",
    "name": f"Product number {i}",
    "qty": (i % 7) + 1,
    "unitPrice": round(5 + (i % 300) * 0.37, 2),
    "currency": "USD",
    "warehouse": ["ams", "fra", "tlv", "nyc"][i % 4],
    "tags": ["bulk", "catalog", f"grp{i % 12}"],
} for i in range(508)]
doc = {
    "batchId": "bulk_2026_10_04",
    "source": "perf-harness",
    "locale": "en-US",
    "items": items,
}
blob = json.dumps(doc, indent=1)
with open(sys.argv[1], "w") as f:
    f.write(blob)
print(f"  json-100kb.json: {len(blob)/1024:.1f} KB, {len(items)} items")
PY

printf '  multipart.bin:   %s bytes\n' "$(wc -c < "${OUT}/multipart.bin" | tr -d ' ')"
echo "Payloads ready in ${PWD}/${OUT}"
