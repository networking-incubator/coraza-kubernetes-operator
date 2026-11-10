#!/usr/bin/env bash

# wrapper for curl, running curl as podman container if CONTAINER_TOOL is set to
# podman.
__original_curl="$(which curl)"

function curl() {
  if [[ $CONTAINER_TOOL =~ "podman" ]]; then
    podman run -it --rm --network=kind quay.io/curl/curl:latest curl $@
  else
    $__original_curl $@
  fi
}

# used for reverting sourced wrapper overrides
function curl_restore() {
    unset curl_restore
    unset __original_curl
    unset curl
}