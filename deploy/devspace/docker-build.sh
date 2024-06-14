#!/usr/bin/env bash

set -xe
DEVSPACE_DEBUG=$1
DOCKER_IMAGE=$2
eval $(go env)
TARGET_PLATFORM=${TARGET_PLATFORM:-${GOHOSTOS}/${GOHOSTARCH}}

if [[ "${DOCKER_IMAGE}" =~ "clickhouse-operator" ]]; then
    DOCKER_FILE=./dockerfile/operator/Dockerfile
else
    DOCKER_FILE=./dockerfile/metrics-exporter/Dockerfile
fi

if [[ "${DEVSPACE_DEBUG}" == "--debug=delve" ]]; then
    time podman buildx build --progress plain  --load --platform="${TARGET_PLATFORM}" -f ${DOCKER_FILE} --target image-debug --build-arg GCFLAGS='all=-N -l' -t "${DOCKER_IMAGE}" .
else
    time podman buildx build --progress plain  --load --platform="${TARGET_PLATFORM}" -f ${DOCKER_FILE} -t "${DOCKER_IMAGE}" .
fi

podman images "${DOCKER_IMAGE%:*}"

if [[ "yes" == "${MINIKUBE}" ]]; then
  minikube image load --daemon=true "${DOCKER_IMAGE}"
fi