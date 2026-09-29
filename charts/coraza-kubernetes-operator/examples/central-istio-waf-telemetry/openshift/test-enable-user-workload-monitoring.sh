#!/usr/bin/env bash
# Smoke tests for the User Workload Monitoring bootstrap helper.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="${script_dir}/01-enable-user-workload-monitoring.sh"
tmpdir="$(mktemp -d)"
trap 'rm -rf "${tmpdir}"' EXIT

mkdir -p "${tmpdir}/bin"

cat >"${tmpdir}/bin/oc" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

case "$1 $2" in
  "get configmap")
    if [[ "${OC_SCENARIO:-missing}" == "existing" ]]; then
      if [[ " $* " == *" -o name "* ]]; then
        printf 'configmap/cluster-monitoring-config'
      else
        printf 'telemeterClient:\n  enabled: true\n'
      fi
      exit 0
    fi
    if [[ " $* " == *" --ignore-not-found "* ]]; then
      exit 0
    fi
    exit 1
    ;;
  "create configmap")
    printf '%s\n' "$*" >>"${OC_CALLS:?}"
    ;;
  "patch configmap")
    printf '%s\n' "$*" >>"${OC_CALLS:?}"
    ;;
  *)
    printf 'unexpected oc invocation: %s\n' "$*" >&2
    exit 1
    ;;
esac
EOF
chmod +x "${tmpdir}/bin/oc"

OC_CALLS="${tmpdir}/calls" PATH="${tmpdir}/bin:${PATH}" "$script"

grep -Fq 'create configmap cluster-monitoring-config -n openshift-monitoring' "${tmpdir}/calls"
grep -Fq -- '--from-literal=config.yaml=enableUserWorkload: true' "${tmpdir}/calls"

: >"${tmpdir}/calls"
OC_SCENARIO=existing OC_CALLS="${tmpdir}/calls" PATH="${tmpdir}/bin:${PATH}" "$script"
grep -Fq 'patch configmap cluster-monitoring-config -n openshift-monitoring' "${tmpdir}/calls"
! grep -Fq 'create configmap' "${tmpdir}/calls"
