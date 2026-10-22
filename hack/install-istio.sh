#!/usr/bin/env bash
#
# install-istio.sh - Install Istio on Kubernetes or OpenShift cluster
#
# Detects the platform and uses the appropriate installation method:
# - OpenShift: Sail Operator (OLM-based)
# - Kubernetes: upstream Istio via istioctl
#
set -euo pipefail

ISTIO_VERSION="${ISTIO_VERSION:-1.30.3}"
SAIL_ISTIO_VERSION="${SAIL_ISTIO_VERSION:-v1.23.2}"
SAIL_CHANNEL="${SAIL_CHANNEL:-3.0-nightly}"

# Detect platform
detect_platform() {
    if kubectl api-resources | grep -q "routes.route.openshift.io"; then
        echo "openshift"
    else
        echo "kubernetes"
    fi
}

# Install Sail Operator on OpenShift
install_sail_operator() {
    echo "==> Installing Sail Operator on OpenShift..."

    # Create istio-system namespace
    kubectl create namespace istio-system --dry-run=client -o yaml | kubectl apply -f -

    # Install Sail Operator via OLM
    cat <<EOF | kubectl apply -f -
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: sail-operator
  namespace: istio-system
spec:
  targetNamespaces:
  - istio-system
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: sailoperator
  namespace: istio-system
spec:
  channel: ${SAIL_CHANNEL}
  name: sailoperator
  source: community-operators
  sourceNamespace: openshift-marketplace
EOF

    echo "==> Waiting for Sail Operator to be ready..."
    kubectl wait --for=condition=Ready pod -l name=sail-operator -n istio-system --timeout=300s || {
        echo "Warning: Timeout waiting for operator pod. Checking CSV status..."
        kubectl get csv -n istio-system
    }

    # Create Istio control plane
    echo "==> Creating Istio control plane (version ${SAIL_ISTIO_VERSION})..."
    cat <<EOF | kubectl apply -f -
apiVersion: sailoperator.io/v1alpha1
kind: Istio
metadata:
  name: default
spec:
  version: ${SAIL_ISTIO_VERSION}
  namespace: istio-system
  updateStrategy:
    type: InPlace
EOF

    echo "==> Waiting for Istio to be ready..."
    kubectl wait --for=condition=Ready istio/default -n istio-system --timeout=300s || {
        echo "Warning: Timeout waiting for Istio. Checking status..."
        kubectl get istio -n istio-system
        kubectl get pods -n istio-system
    }

    echo "==> Sail Operator installation complete"
    kubectl get istio -n istio-system
    kubectl get pods -n istio-system
}

# Install upstream Istio via istioctl
install_istioctl() {
    echo "==> Installing upstream Istio ${ISTIO_VERSION} on Kubernetes..."

    # Check if istioctl is available
    if ! command -v istioctl &>/dev/null; then
        echo "==> Downloading istioctl ${ISTIO_VERSION}..."
        curl -L https://istio.io/downloadIstio | ISTIO_VERSION=${ISTIO_VERSION} sh -
        export PATH="$PWD/istio-${ISTIO_VERSION}/bin:$PATH"
    fi

    echo "==> Installing Istio with demo profile..."
    istioctl install --set profile=demo -y

    echo "==> Istio installation complete"
    kubectl get pods -n istio-system
}

# Main
main() {
    PLATFORM=$(detect_platform)
    echo "==> Detected platform: ${PLATFORM}"

    case "${PLATFORM}" in
        openshift)
            install_sail_operator
            ;;
        kubernetes)
            install_istioctl
            ;;
        *)
            echo "Error: Unknown platform: ${PLATFORM}"
            exit 1
            ;;
    esac

    echo ""
    echo "✅ Istio installation complete"
    echo ""
    echo "Next steps:"
    echo "  1. Deploy Coraza operator: make deploy"
    echo "  2. Create RuleSets and Engines"
}

main "$@"
