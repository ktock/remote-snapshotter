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

if [ "${1}" == "" ] ; then
    echo "Specify directory for output"
    exit 1
fi

DATADIR="${1}/"
echo "output into: ${DATADIR}"

MODES=( ${TARGET_MODES:-} )
if [ ${#MODES[@]} -eq 0 ] ; then
    MODES=("legacy" "estargz-noopt" "estargz" "zstdchunked")
fi

IMAGES=( ${TARGET_IMAGES:-} )
if [ ${#IMAGES[@]} -eq 0 ] ; then
    IMAGES=( $(cat "${JSON}" | jq -r '[ .[] | select(.mode=="'${MODES[0]}'").repo ] | unique[]') )
fi

GRANULARITY="${BENCHMARK_PERCENTILES_GRANULARITY}"
if [ "${GRANULARITY}" == "" ] ; then
    GRANULARITY="0.1"
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

function template {
    local GRAPHFILE="${1}"
    local IMGNAME="${2}"
    local MODE="${3}"
    local OPERATION="${4}"
    local SAMPLES="${5}"

    cat <<EOF
set output '${GRAPHFILE}'
set title "${OPERATION} of ${IMGNAME}/${MODE} (${SAMPLES}samples)"
set terminal png size 500, 375
set boxwidth 0.5 relative
set style fill solid 1.0 border -1
set xlabel 'percentile'
set ylabel 'time[sec]'
EOF
}

RAWDATADIR="${DATADIR}raw/"
PLTDATADIR="${DATADIR}plt/"
IMGDATADIR="${DATADIR}png/"
CSVDATADIR="${DATADIR}csv/"
mkdir "${RAWDATADIR}" "${PLTDATADIR}" "${IMGDATADIR}" "${CSVDATADIR}"
for IMGNAME in "${IMAGES[@]}" ; do
    echo "Processing: ${IMGNAME}"
    IMAGE=$(echo "${IMGNAME}" | sed 's|[/:]|-|g')
    CSVFILE="${CSVDATADIR}result-${IMAGE}.csv"
    echo "image,mode,operation,percentile,time" > "${CSVFILE}"
    for MODE in "${MODES[@]}"; do
        for OPERATION in "pull" "create" "run" ; do
            DATAFILE="${RAWDATADIR}${IMAGE}-${MODE}-${OPERATION}.dat"
            PLTFILE="${PLTDATADIR}result-${IMAGE}-${MODE}-${OPERATION}.plt"
            template "${IMGDATADIR}result-${IMAGE}-${MODE}-${OPERATION}.png" \
                     "${IMGNAME}" "${MODE}" "${OPERATION}" "${MINSAMPLES}" > "${PLTFILE}"
            for PCTL in $(seq 0 "${GRANULARITY}" 100) ; do
                TIME=$(PERCENTILE=${PCTL} percentile "${JSON}" "${MINSAMPLES}" "${IMGNAME}" "${MODE}" "elapsed_${OPERATION}")
                echo "${PCTL} ${TIME}" >> "${DATAFILE}"
                echo "${IMGNAME},${MODE},${OPERATION},${PCTL},${TIME}" >> "${CSVFILE}"
            done
            echo 'plot "'"${DATAFILE}"'" using 0:2:xtic(1) with boxes lw 1 lc rgb "black" notitle' >> "${PLTFILE}"
            gnuplot "${PLTFILE}"
        done
    done
done
