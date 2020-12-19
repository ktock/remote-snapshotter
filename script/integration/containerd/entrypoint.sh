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

# NOTE: The entire contents of containerd/stargz-snapshotter are located in
# the testing container so utils.sh is visible from this script during runtime.
# TODO: Refactor the code dependencies and pack them in the container without
#       expecting and relying on volumes.
source "/utils.sh"

PLUGIN=stargz
REGISTRY_HOST=registry-integration.test
REGISTRY_ALT_HOST=registry-alt.test
DUMMYUSER=dummyuser
DUMMYPASS=dummypass

USR_ORG=$(mktemp -d)
USR_MIRROR=$(mktemp -d)
USR_REFRESH=$(mktemp -d)
USR_NORMALSN_UNSTARGZ=$(mktemp -d)
USR_STARGZSN_UNSTARGZ=$(mktemp -d)
USR_NORMALSN_STARGZ=$(mktemp -d)
USR_STARGZSN_STARGZ=$(mktemp -d)
USR_NORMALSN_ZSTD=$(mktemp -d)
USR_STARGZSN_ZSTD=$(mktemp -d)
USR_NORMALSN_PLAIN_STARGZ=$(mktemp -d)
USR_STARGZSN_PLAIN_STARGZ=$(mktemp -d)
LOG_FILE=$(mktemp)
function cleanup {
    ORG_EXIT_CODE="${1}"
    rm -rf "${USR_ORG}" || true
    rm -rf "${USR_MIRROR}" || true
    rm -rf "${USR_REFRESH}" || true
    rm -rf "${USR_NORMALSN_UNSTARGZ}" || true
    rm -rf "${USR_STARGZSN_UNSTARGZ}" || true
    rm -rf "${USR_NORMALSN_STARGZ}" || true
    rm -rf "${USR_STARGZSN_STARGZ}" || true
    rm -rf "${USR_NORMALSN_PLAIN_STARGZ}" || true
    rm -rf "${USR_STARGZSN_PLAIN_STARGZ}" || true
    rm -rf "${USR_NORMALSN_ZSTD}" || true
    rm -rf "${USR_STARGZSN_ZSTD}" || true
    rm "${LOG_FILE}"
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

RETRYNUM=100
RETRYINTERVAL=1
TIMEOUTSEC=180
function retry {
    local SUCCESS=false
    for i in $(seq ${RETRYNUM}) ; do
        if eval "timeout ${TIMEOUTSEC} ${@}" ; then
            SUCCESS=true
            break
        fi
        echo "Fail(${i}). Retrying..."
        sleep ${RETRYINTERVAL}
    done
    if [ "${SUCCESS}" == "true" ] ; then
        return 0
    else
        return 1
    fi
}

function kill_all {
    if [ "${1}" != "" ] ; then
        ps aux | grep "${1}" | grep -v grep | grep -v $(basename ${0}) | sed -E 's/ +/ /g' | cut -f 2 -d ' ' | xargs -I{} kill -9 {} || true
    fi
}

CONTAINERD_ROOT=/var/lib/containerd/
CONTAINERD_STATUS=/run/containerd/
REMOTE_SNAPSHOTTER_SOCKET=/run/containerd-stargz-grpc/containerd-stargz-grpc.sock
REMOTE_SNAPSHOTTER_ROOT=/var/lib/containerd-stargz-grpc/
function reboot_containerd {
    kill_all "containerd"
    kill_all "containerd-stargz-grpc"
    rm -rf "${CONTAINERD_STATUS}"*
    rm -rf "${CONTAINERD_ROOT}"*
    if [ -f "${REMOTE_SNAPSHOTTER_SOCKET}" ] ; then
        rm "${REMOTE_SNAPSHOTTER_SOCKET}"
    fi
    if [ -d "${REMOTE_SNAPSHOTTER_ROOT}snapshotter/snapshots/" ] ; then 
        find "${REMOTE_SNAPSHOTTER_ROOT}snapshotter/snapshots/" \
             -maxdepth 1 -mindepth 1 -type d -exec umount "{}/fs" \;
    fi
    rm -rf "${REMOTE_SNAPSHOTTER_ROOT}"*
    if [ "${BUILTIN_SNAPSHOTTER}" == "true" ] ; then
        if [ "${CONTAINERD_CONFIG:-}" != "" ] ; then
            containerd --log-level debug --config="${CONTAINERD_CONFIG:-}" 2>&1 | tee -a "${LOG_FILE}" &
        else
            containerd --log-level debug --config=/etc/containerd/config.toml 2>&1 | tee -a "${LOG_FILE}" &
        fi
    else
        if [ "${SNAPSHOTTER_CONFIG:-}" == "" ] ; then
            containerd-stargz-grpc --log-level=debug \
                                   --address="${REMOTE_SNAPSHOTTER_SOCKET}" \
                                   2>&1 | tee -a "${LOG_FILE}" & # Dump all log
        else
            containerd-stargz-grpc --log-level=debug \
                                   --address="${REMOTE_SNAPSHOTTER_SOCKET}" \
                                   --config="${SNAPSHOTTER_CONFIG}" \
                                   2>&1 | tee -a "${LOG_FILE}" &
        fi
        retry ls "${REMOTE_SNAPSHOTTER_SOCKET}"
        containerd --log-level debug --config=/etc/containerd/config.toml &
    fi

    # Makes sure containerd and containerd-stargz-grpc are up-and-running.
    UNIQUE_SUFFIX=$(date +%s%N | shasum | base64 | fold -w 10 | head -1)
    retry ctr snapshots --snapshotter="${PLUGIN}" prepare "connectiontest-dummy-${UNIQUE_SUFFIX}" ""
}

function optimize {
    local SRC="${1}"
    local DST="${2}"
    local PUSHOPTS=${@:3}
    ctr-remote image pull -u "${DUMMYUSER}:${DUMMYPASS}" "${SRC}"
    ctr-remote image optimize --oci "${SRC}" "${DST}"
    ctr-remote image push ${PUSHOPTS} -u "${DUMMYUSER}:${DUMMYPASS}" "${DST}"
}

function convert {
    local SRC="${1}"
    local DST="${2}"
    local PUSHOPTS=${@:3}
    ctr-remote image pull -u "${DUMMYUSER}:${DUMMYPASS}" "${SRC}"
    ctr-remote image optimize --no-optimize "${SRC}" "${DST}"
    ctr-remote image push ${PUSHOPTS} -u "${DUMMYUSER}:${DUMMYPASS}" "${DST}"
}

function copy {
    local SRC="${1}"
    local DST="${2}"
    ctr-remote i pull --all-platforms "${SRC}"
    ctr-remote i tag "${SRC}" "${DST}"
    ctr-remote i push -u "${DUMMYUSER}:${DUMMYPASS}" "${DST}"
}

function dump_dir {
    local IMAGE="${1}"
    local TARGETDIR="${2}"
    local SNAPSHOTTER="${3}"
    local REMOTE="${4}"
    local DEST="${5}"

    reboot_containerd
    if [ "${REMOTE}" == "true" ] ; then
        echo -n "" > "${LOG_FILE}"
        ctr-remote images rpull --user "${DUMMYUSER}:${DUMMYPASS}" "${IMAGE}"
        check_remote_snapshots "${LOG_FILE}"
    else
        ctr-remote images pull --snapshotter="${SNAPSHOTTER}" --user "${DUMMYUSER}:${DUMMYPASS}" "${IMAGE}"
    fi
    ctr-remote run --rm --snapshotter="${SNAPSHOTTER}" "${IMAGE}" test tar -c "${TARGETDIR}" \
        | tar -xC "${DEST}"
}

echo "===== VERSION INFORMATION ====="
containerd --version
runc --version
echo "==============================="

cat <<EOF >> /etc/containerd/config.toml
[debug]
format = "json"
level = "debug"
EOF
if [ "${BUILTIN_SNAPSHOTTER}" != "true" ] ; then
    cat <<EOF >> /etc/containerd/config.toml
[proxy_plugins]
  [proxy_plugins.stargz]
    type = "snapshot"
    address = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"
EOF
fi

echo "Logging into the registry..."
cp /auth/certs/domain.crt /usr/local/share/ca-certificates
update-ca-certificates
retry nerdctl login -u "${DUMMYUSER}" -p "${DUMMYPASS}" "${REGISTRY_HOST}"

reboot_containerd
OK=$(ctr-remote plugins ls \
         | grep io.containerd.snapshotter \
         | sed -E 's/ +/ /g' \
         | cut -d ' ' -f 2,4 \
         | grep "${PLUGIN}" \
         | cut -d ' ' -f 2)
if [ "${OK}" != "ok" ] ; then
    echo "Plugin ${PLUGIN} not found" 1>&2
    exit 1
fi

echo "Preparing images..."
copy docker.io/library/ubuntu:18.04 "${REGISTRY_HOST}/ubuntu:18.04"
copy docker.io/library/alpine:3.10.2 "${REGISTRY_HOST}/alpine:3.10.2"
stargzify "${REGISTRY_HOST}/ubuntu:18.04" "${REGISTRY_HOST}/ubuntu:sgz"
optimize "${REGISTRY_HOST}/ubuntu:18.04" "${REGISTRY_HOST}/ubuntu:esgz"
optimize "${REGISTRY_HOST}/ubuntu:18.04" "${REGISTRY_HOST}/ubuntu:zstdchunked"
optimize "${REGISTRY_HOST}/alpine:3.10.2" "${REGISTRY_HOST}/alpine:esgz"
optimize "${REGISTRY_HOST}/alpine:3.10.2" "${REGISTRY_ALT_HOST}:5000/alpine:esgz" --plain-http

############
# Tests for refreshing and mirror
echo "Testing refreshing and mirror..."

reboot_containerd
echo "Getting image with normal snapshotter..."
ctr-remote images pull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}/alpine:esgz"
ctr-remote run --rm "${REGISTRY_HOST}/alpine:esgz" test tar -c /usr | tar -xC "${USR_ORG}"

echo "Getting image with stargz snapshotter..."
echo -n "" > "${LOG_FILE}"
ctr-remote images rpull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}/alpine:esgz"
check_remote_snapshots "${LOG_FILE}"

REGISTRY_HOST_IP=$(getent hosts "${REGISTRY_HOST}" | awk '{ print $1 }')
REGISTRY_ALT_HOST_IP=$(getent hosts "${REGISTRY_ALT_HOST}" | awk '{ print $1 }')

echo "Disabling source registry and check if mirroring is working for stargz snapshotter..."
iptables -A OUTPUT -d "${REGISTRY_HOST_IP}" -j DROP
iptables -L
ctr-remote run --rm --snapshotter=stargz "${REGISTRY_HOST}/alpine:esgz" test tar -c /usr \
    | tar -xC "${USR_MIRROR}"
iptables -D OUTPUT -d "${REGISTRY_HOST_IP}" -j DROP

echo "Disabling mirror registry and check if refreshing works for stargz snapshotter..."
iptables -A OUTPUT -d "${REGISTRY_ALT_HOST_IP}" -j DROP
iptables -L
ctr-remote run --rm --snapshotter=stargz "${REGISTRY_HOST}/alpine:esgz" test tar -c /usr \
    | tar -xC "${USR_REFRESH}"
iptables -D OUTPUT -d "${REGISTRY_ALT_HOST_IP}" -j DROP

echo "Disabling all registries and running container should fail"
iptables -A OUTPUT -d "${REGISTRY_HOST_IP}","${REGISTRY_ALT_HOST_IP}" -j DROP
iptables -L
if ctr-remote run --rm --snapshotter=stargz "${REGISTRY_HOST}/alpine:esgz" test tar -c /usr > /usr_dummy_fail.tar ; then
    echo "All registries are disabled so this must be failed"
    exit 1
else
    echo "Failed to run the container as expected"
fi
iptables -D OUTPUT -d "${REGISTRY_HOST_IP}","${REGISTRY_ALT_HOST_IP}" -j DROP

echo "Diffing root filesystems for mirroring"
diff --no-dereference -qr "${USR_ORG}/" "${USR_MIRROR}/"

echo "Diffing root filesystems for refreshing"
diff --no-dereference -qr "${USR_ORG}/" "${USR_REFRESH}/"

############
# Tests for stargz filesystem
echo "Testing stargz filesystem..."

# Test with a normal image

echo "Getting normal image with normal snapshotter..."
dump_dir "${REGISTRY_HOST}/ubuntu:18.04" "/usr" "overlayfs" "false" "${USR_NORMALSN_UNSTARGZ}"

echo "Getting normal image with stargz snapshotter..."
dump_dir "${REGISTRY_HOST}/ubuntu:18.04" "/usr" "stargz" "false" "${USR_STARGZSN_UNSTARGZ}"

echo "Diffing bitween two root filesystems(normal vs stargz snapshotter, normal rootfs)"
diff --no-dereference -qr "${USR_NORMALSN_UNSTARGZ}/" "${USR_STARGZSN_UNSTARGZ}/"

# Test with an eStargz image

echo "Getting eStargz image with normal snapshotter..."
dump_dir "${REGISTRY_HOST}/ubuntu:esgz" "/usr" "overlayfs" "false" "${USR_NORMALSN_STARGZ}"

echo "Getting eStargz image with stargz snapshotter..."
dump_dir "${REGISTRY_HOST}/ubuntu:esgz" "/usr" "stargz" "true" "${USR_STARGZSN_STARGZ}"

echo "Diffing bitween two root filesystems(normal vs stargz snapshotter, eStargz rootfs)"
diff --no-dereference -qr "${USR_NORMALSN_STARGZ}/" "${USR_STARGZSN_STARGZ}/"

# Test with a zstd:chunked image

echo "Getting zstd:chunked image with normal snapshotter..."
dump_dir "${REGISTRY_HOST}/ubuntu:zstdchunked" "/usr" "overlayfs" "false" "${USR_NORMALSN_ZSTD}"

echo "Getting zstd:chunked image with stargz snapshotter..."
dump_dir "${REGISTRY_HOST}/ubuntu:zstdchunked" "/usr" "stargz" "true" "${USR_STARGZSN_ZSTD}"

echo "Diffing bitween two root filesystems(normal vs stargz snapshotter, zstd:cunked rootfs)"
diff --no-dereference -qr "${USR_NORMALSN_ZSTD}/" "${USR_STARGZSN_ZSTD}/"

############
# Checking compatibility with plain stargz

reboot_containerd
echo "Getting (legacy) stargz image with normal snapshotter..."
dump_dir "${REGISTRY_HOST}/ubuntu:sgz" "/usr" "overlayfs" "false" "${USR_NORMALSN_PLAIN_STARGZ}"

echo "Getting (legacy) stargz image with stargz snapshotter..."
TEST_CONTAINERD_CONFIG=
TEST_SNAPSHOTTER_CONFIG=
if [ "${BUILTIN_SNAPSHOTTER}" == "true" ] ; then
    cp /etc/containerd/config.toml /tmp/config.containerd.noverify.toml
    sed -i 's/disable_verification = false/disable_verification = true/g' /tmp/config.containerd.noverify.toml
    TEST_CONTAINERD_CONFIG="/tmp/config.containerd.noverify.toml"
else
    echo "disable_verification = true" > /tmp/config.stargz.noverify.toml
    cat /etc/containerd-stargz-grpc/config.toml >> /tmp/config.stargz.noverify.toml
    TEST_SNAPSHOTTER_CONFIG="/tmp/config.stargz.noverify.toml"
fi

CONTAINERD_CONFIG="${TEST_CONTAINERD_CONFIG}" SNAPSHOTTER_CONFIG="${TEST_SNAPSHOTTER_CONFIG}" \
                 dump_dir "${REGISTRY_HOST}/ubuntu:sgz" "/usr" "stargz" "true" "${USR_STARGZSN_PLAIN_STARGZ}"

echo "Diffing bitween two root filesystems(normal vs stargz snapshotter, plain stargz rootfs)"
diff --no-dereference -qr "${USR_NORMALSN_PLAIN_STARGZ}/" "${USR_STARGZSN_PLAIN_STARGZ}/"

############
# Try to pull this image from different namespace.
ctr-remote --namespace=dummy images rpull --user "${DUMMYUSER}:${DUMMYPASS}" \
           "${REGISTRY_HOST}/ubuntu:esgz"

############
# Test for starting when no configuration file.
mv /etc/containerd-stargz-grpc/config.toml /etc/containerd-stargz-grpc/config.toml_rm
reboot_containerd
mv /etc/containerd-stargz-grpc/config.toml_rm /etc/containerd-stargz-grpc/config.toml

exit 0
