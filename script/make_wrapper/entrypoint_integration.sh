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

PLUGINS=(remote stargz)
REGISTRY_HOST=registry_integration
DUMMYUSER=dummyuser
DUMMYPASS=dummypass

RETRYNUM=30
RETRYINTERVAL=1
TIMEOUTSEC=180
function retry {
    for i in $(seq ${RETRYNUM}) ; do
        if eval "timeout ${TIMEOUTSEC} ${@}" ; then
            break
        fi
        echo "Fail(${i}). Retrying..."
        sleep ${RETRYINTERVAL}
    done
    if [ ${i} -eq ${RETRYNUM} ] ; then
        return 1
    else
        return 0
    fi
}

function check {
    if [ ${?} = 0 ] ; then
        echo "Completed: ${1}"
    else
        echo "Failed: ${1}"
        exit 1
    fi
}

function isServedAsRemoteSnapshot {
    LOG_PATH="${1}"
    if [ "$(cat ${LOG_PATH})" == "" ] ; then
        echo "Log is empty. Something is wrong."
        return 1
    fi

    LAYER_LOG=$(cat "${LOG_PATH}" | grep "layer-sha256:")
    if [ "${LAYER_LOG}" != "" ] ; then
        echo "Some layer have been downloaded by containerd"
        return 1
    fi
    return 0
}

CONTAINERD_ROOT=/var/lib/containerd/
function reboot_containerd {
    ps aux | grep containerd | grep -v grep | sed -E 's/ +/ /g' | cut -f 2 -d ' ' | xargs -I{} kill -9 {}
    ls -1d "${CONTAINERD_ROOT}io.containerd.snapshotter.v1.${SNAPSHOT_NAME}/snapshots/"* | xargs -I{} echo "{}/fs" | xargs -I{} umount {}
    rm -rf "${CONTAINERD_ROOT}"*
    containerd ${@} &
    retry ctr version
}

# Login to the registry
cp /auth/certs/domain.crt /usr/local/share/ca-certificates
check "Importing cert"

update-ca-certificates
check "Installing cert"

retry docker login "${REGISTRY_HOST}:5000" -u "${DUMMYUSER}" -p "${DUMMYPASS}"
check "Login to the registry"

# Install remote snapshotter
cd /go/src/github.com/ktock/remote-snapshotter
GO111MODULE=off make clean && GO111MODULE=off make -j2 && GO111MODULE=off make install
check "Installing remote snapshotter"

reboot_containerd --log-level debug --config=/etc/containerd/config.integration.stargz.toml

############
# Tests for remote snapshotter
NOTFOUND=false
for PLUGIN in ${PLUGINS[@]}; do
    OK=$(ctr plugins ls \
             | grep io.containerd.snapshotter \
             | sed -E 's/ +/ /g' \
             | cut -d ' ' -f 2,4 \
             | grep "${PLUGIN}" \
             | cut -d ' ' -f 2)
    if [ "${OK}" != "ok" ] ; then
        echo "Plugin ${PLUGIN} not found" 1>&2
        NOTFOUND=true
    fi
done

if [ "${NOTFOUND}" != "false" ] ; then
    exit 1
fi

############
# Tests for stargz filesystem
stargzify ubuntu:18.04 "${REGISTRY_HOST}:5000/ubuntu:18.04"
check "Stargzify"

reboot_containerd --log-level debug --config=/etc/containerd/config.integration.stargz.toml
ctr images pull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}:5000/ubuntu:18.04"
check "Getting original image"
ctr run --rm "${REGISTRY_HOST}:5000/ubuntu:18.04" test tar -c /usr > /usr1.tar

PULL_LOG=$(mktemp)
check "Preparing log file"
reboot_containerd --log-level debug --config=/etc/containerd/config.integration.stargz.toml
ctr images pull --user "${DUMMYUSER}:${DUMMYPASS}" --skip-download --snapshotter=remote "${REGISTRY_HOST}:5000/ubuntu:18.04" | tee "${PULL_LOG}"
check "Getting image lazily"
if ! isServedAsRemoteSnapshot "${PULL_LOG}" ; then
    echo "Failed to serve all layers as remote snapshots: ${LAYER_LOG}"
    exit 1
fi
rm "${PULL_LOG}"
ctr run --rm --snapshotter=remote "${REGISTRY_HOST}:5000/ubuntu:18.04" test tar -c /usr > /usr2.tar

mkdir /usr1 /usr2 && tar -xf /usr1.tar -C /usr1 && tar -xf /usr2.tar -C /usr2
check "Diff preparation"

diff --no-dereference -qr /usr1/ /usr2/
check "Diff bitween two root filesystems(normal rootfs vs lazypull-ed rootfs)"

exit 0

