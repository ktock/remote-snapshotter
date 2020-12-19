#!/bin/bash

#   Copyright The containerd Authors.

#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at

#       http://www.apache.org/licenses/LICENSE-2.0

#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.

set -euo pipefail

JSON="$(mktemp)"
cat > "${JSON}"

CONTEXT="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )/"
source "${CONTEXT}/util.sh"

MODES=( ${TARGET_MODES:-} )
if [ ${#MODES[@]} -eq 0 ] ; then
    MODES=("legacy" "estargz-noopt" "estargz" "zstdchunked")
fi

IMAGES=( ${TARGET_IMAGES:-} )
if [ ${#IMAGES[@]} -eq 0 ] ; then
    IMAGES=( $(cat "${JSON}" | jq -r '[ .[] | select(.mode=="'${MODES[0]}'").repo ] | unique[]') )
fi

# Ensure we use the exact same number of samples among benchmarks
MINSAMPLES=
for IMGNAME in "${IMAGES[@]}" ; do
    for MODE in "${MODES[@]}"; do
        THEMIN=$(min_samples "${JSON}" "${IMGNAME}" "${MODE}")
        if [ "${MINSAMPLES}" == "" ] ; then
            MINSAMPLES="${THEMIN}"
        fi
        MINSAMPLES=$(echo "${MINSAMPLES} ${THEMIN}" | tr ' ' '\n' | sort -n | head -1)
    done
done

INDEX="image,operation"
for MODE in "${MODES[@]}"; do
    INDEX="${INDEX},${MODE}"
done
echo "${INDEX}"
for IMGNAME in "${IMAGES[@]}" ; do
    PULLLINE="${IMGNAME},pull"
    for MODE in "${MODES[@]}"; do
        PULLTIME=$(percentile "${JSON}" "${MINSAMPLES}" "${IMGNAME}" "${MODE}" "elapsed_pull")
        PULLLINE="${PULLLINE},${PULLTIME}"
    done
    echo "${PULLLINE}"

    CREATELINE="${IMGNAME},create"
    for MODE in "${MODES[@]}"; do
        CREATETIME=$(percentile "${JSON}" "${MINSAMPLES}" "${IMGNAME}" "${MODE}" "elapsed_create")
        CREATELINE="${CREATELINE},${CREATETIME}"
    done
    echo "${CREATELINE}"

    RUNLINE="${IMGNAME},run"
    for MODE in "${MODES[@]}"; do
        RUNTIME=$(percentile "${JSON}" "${MINSAMPLES}" "${IMGNAME}" "${MODE}" "elapsed_run")
        RUNLINE="${RUNLINE},${RUNTIME}"
    done
    echo "${RUNLINE}"
done
