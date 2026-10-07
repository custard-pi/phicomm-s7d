#!/bin/ash
# Download a published ARM64 release and install it on OpenWrt/ImmortalWrt.
# Fill in owner/repository after publishing, or export GITHUB_REPO before running.
GITHUB_REPO="${GITHUB_REPO:-custard-pi/phicomm-s7d}"
RELEASE_TAG="${RELEASE_TAG:-latest}"

set -eu
umask 077

die() {
    echo "Error: $*" >&2
    exit 1
}

case "$GITHUB_REPO" in
    ''|*[!A-Za-z0-9_./-]*) die 'Set GITHUB_REPO to your GitHub owner/repository.' ;;
esac
case "$GITHUB_REPO" in
    */*) ;;
    *) die 'GITHUB_REPO must be owner/repository.' ;;
esac
owner=${GITHUB_REPO%%/*}
repository=${GITHUB_REPO#*/}
case "$owner:$repository" in
    :*|*:|*/*|.*|*:.*) die 'GITHUB_REPO must be owner/repository.' ;;
esac

[ "$(id -u)" = 0 ] || die 'Run this script as root on OpenWrt.'
[ -f /etc/rc.common ] || die 'OpenWrt rc.common was not found.'
case "$(uname -m)" in
    aarch64|arm64) ;;
    *) die 'This installer supports ARM64 (aarch64) only.' ;;
esac
command -v sha256sum >/dev/null 2>&1 || die 'sha256sum is required.'
command -v jsonfilter >/dev/null 2>&1 || die 'jsonfilter is required.'
command -v uci >/dev/null 2>&1 || die 'uci is required.'
[ -x /etc/init.d/firewall ] || die 'The OpenWrt firewall service was not found.'
[ -t 0 ] || die 'Run this downloaded script in an interactive SSH terminal.'
pending_firewall=$(uci changes firewall) || die 'Cannot read firewall configuration.'
[ -z "$pending_firewall" ] || die 'Commit or revert pending firewall changes before installing.'

ask() {
    printf '%s [%s]: ' "$1" "$2" >&2
    IFS= read -r reply || die 'Input ended; installation cancelled.'
    printf '%s\n' "${reply:-$2}"
}

valid_port() {
    case "$1" in ''|*[!0-9]*) return 1 ;; esac
    [ "${#1}" -le 5 ] && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]
}

valid_ipv4() {
    case "$1" in ''|*[!0-9.]*) return 1 ;; esac
    printf '%s\n' "$1" | awk -F. '
        NF != 4 {exit 1}
        {for (i=1; i<=4; i++) if ($i == "" || length($i)>3 || $i>255 || (length($i)>1 && substr($i,1,1)=="0")) exit 1}'
}

download() {
    if command -v curl >/dev/null 2>&1; then
        curl -fL --retry 3 --connect-timeout 15 --max-time 180 -o "$2" "$1"
    elif command -v uclient-fetch >/dev/null 2>&1; then
        uclient-fetch -O "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -O "$2" "$1"
    else
        die 'Install curl, uclient-fetch, or wget with HTTPS support.'
    fi
}

binary=/usr/bin/phicomm-s7d
service=/etc/init.d/phicomm-s7d
config=/etc/phicom-s7/config.json
binary_stage=/usr/bin/.phicomm-s7d.install-$$
service_stage=/etc/init.d/.phicomm-s7d.install-$$
config_stage=/etc/phicom-s7/.config.install-$$
workdir=$(mktemp -d /tmp/phicomm-s7-install.XXXXXX)
rollback_needed=0
config_changed=0
firewall_changed=0
firewall_backup=
was_running=0
was_enabled=0

cleanup() {
    result=$?
    trap - 0 HUP INT TERM
    set +e
    if [ "$rollback_needed" = 1 ]; then
        echo 'Installation failed; restoring the previous program and service.' >&2
        [ ! -x "$service" ] || "$service" stop >/dev/null 2>&1
        [ ! -x "$service" ] || "$service" disable >/dev/null 2>&1
        if [ "$firewall_changed" = 1 ]; then
            if uci revert firewall && uci import firewall < "$firewall_backup" &&
                uci commit firewall && /etc/init.d/firewall reload; then
                echo 'Previous firewall configuration restored.' >&2
            else
                echo "Firewall restore failed. Backup: $firewall_backup" >&2
            fi
        fi
        if [ "$config_changed" = 1 ]; then
            if [ -f "$workdir/previous-config" ]; then
                cp -p "$workdir/previous-config" "$config_stage" && mv -f "$config_stage" "$config"
            else
                rm -f "$config"
            fi
        fi
        if [ -f "$workdir/previous-binary" ]; then
            cp -p "$workdir/previous-binary" "$binary_stage" && mv -f "$binary_stage" "$binary"
        else
            rm -f "$binary"
        fi
        if [ -f "$workdir/previous-service" ]; then
            cp -p "$workdir/previous-service" "$service_stage" && mv -f "$service_stage" "$service"
            [ "$was_enabled" != 1 ] || "$service" enable
            [ "$was_running" != 1 ] || "$service" start
        else
            rm -f "$service"
        fi
    fi
    rm -f "$binary_stage" "$service_stage" "$config_stage"
    # Only remove files created by this installer in the mktemp directory.
    rm -f "$workdir/release.json" \
        "$workdir/phicomm-s7d-linux-arm64.sha256sum" "$workdir/phicomm-s7d.init.sha256sum" \
        "$workdir/openwrt-firewall-setup.sh.sha256sum" \
        "$workdir/phicomm-s7d-linux-arm64" "$workdir/phicomm-s7d.init" \
        "$workdir/openwrt-firewall-setup.sh" \
        "$workdir/previous-binary" "$workdir/previous-service" \
        "$workdir/previous-config" "$workdir/config.json"
    rmdir "$workdir"
    exit "$result"
}
trap cleanup 0
trap 'exit 1' HUP INT TERM

# Collect every answer before downloading or changing the installation.
echo 'Phicomm S7 setup: answer all questions below, then installation runs automatically.'
keep_config=n
if [ -e "$config" ]; then
    keep_config=$(ask 'Keep existing program configuration? (y/n)' y)
fi
case "$keep_config" in
    y|Y|yes|YES)
        keep_config=y
        tcp_listen=$(jsonfilter -i "$config" -e '@.tcp_listen')
        http_listen=$(jsonfilter -i "$config" -e '@.http_listen')
        tcp_port=${tcp_listen##*:}
        http_port=${http_listen##*:}
        ;;
    n|N|no|NO)
        keep_config=n
        tcp_port=$(ask 'Local S7 TCP port' 30101)
        http_port=$(ask 'Web HTTP port' 8088)
        data_file=$(ask 'Measurement JSONL path' /etc/phicom-s7/measurements.jsonl)
        height=$(ask 'Height in cm' 180)
        coef_set=$(ask 'Regression coefficient set (male/female)' male)
        case "$data_file" in /?*) ;; *) die 'Measurement path must be an absolute file path.' ;; esac
        printf '%s\n' "$height" | awk '/^[0-9]+([.][0-9]+)?$/ && $0>=50 && $0<=250 {ok=1} END {exit !ok}' || die 'Height must be between 50 and 250 cm.'
        case "$coef_set" in male|female) ;; *) die 'Coefficient set must be male or female.' ;; esac
        tcp_listen=:$tcp_port
        http_listen=:$http_port
        ;;
    *) die 'Answer y or n for keeping configuration.' ;;
esac
valid_port "$tcp_port" && valid_port "$http_port" || die 'Listen ports must be between 1 and 65535.'
[ "$tcp_port" != "$http_port" ] || die 'S7 and HTTP ports must differ.'

server_default=$(uci -q get firewall.hijack_phicomm_s7_dnat.dest_ip || :)
scale_default=$(uci -q get firewall.hijack_phicomm_s7_dnat.src_ip || :)
cloud_default=$(uci -q get firewall.hijack_phicomm_s7_dnat.src_dip || :)
port_default=$(uci -q get firewall.hijack_phicomm_s7_dnat.src_dport || :)
mac_default=$(uci -q get firewall.hijack_phicomm_s7_dnat.src_mac || :)
source_default=$(uci -q get firewall.hijack_phicomm_s7_dnat.src || :)
dest_default=$(uci -q get firewall.hijack_phicomm_s7_dnat.dest || :)
server_ip=$(ask 'This router/server LAN IPv4 address' "${server_default:-192.168.1.1}")
scale_ip=$(ask 'Phicomm S7 IPv4 address' "${scale_default:-192.168.1.2}")
cloud_ip=$(ask 'Original cloud IPv4 address' "${cloud_default:-106.14.93.199}")
cloud_port=$(ask 'Original cloud TCP port' "${port_default:-30101}")
scale_mac=$(ask 'S7 MAC (optional; - clears an existing MAC)' "$mac_default")
[ "$scale_mac" != - ] || scale_mac=
source_zone=$(ask 'S7 source firewall zone' "${source_default:-lan}")
dest_zone=$(ask 'Server destination firewall zone' "${dest_default:-lan}")
for address in "$server_ip" "$scale_ip" "$cloud_ip"; do
    valid_ipv4 "$address" || die "Invalid IPv4 address: $address"
done
[ "$server_ip" != "$scale_ip" ] || die 'The server and scale must have different addresses.'
valid_port "$cloud_port" || die 'Cloud port must be between 1 and 65535.'
if [ -n "$scale_mac" ]; then
    printf '%s\n' "$scale_mac" | grep -Eq '^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$' || die 'Invalid MAC address.'
fi
for zone in "$source_zone" "$dest_zone"; do
    case "$zone" in ''|*[!A-Za-z0-9_-]*) die "Invalid firewall zone: $zone" ;; esac
done
echo
echo "Program configuration: $config (keep existing: $keep_config)"
echo "S7: $scale_ip${scale_mac:+ ($scale_mac)}"
echo "Redirect: $cloud_ip:$cloud_port -> $server_ip:$tcp_port ($source_zone -> $dest_zone)"
echo "Web listen: $http_listen"
confirm=$(ask 'Install, enable service, and apply DNAT + SNAT? (y/n)' y)
case "$confirm" in y|Y|yes|YES) ;; *) echo 'Cancelled; no changes were made.'; exit 0 ;; esac

if [ "$RELEASE_TAG" = latest ]; then
    download "https://api.github.com/repos/$GITHUB_REPO/releases/latest" "$workdir/release.json"
    RELEASE_TAG=$(jsonfilter -i "$workdir/release.json" -e '@.tag_name')
fi
case "$RELEASE_TAG" in
    ''|*[!A-Za-z0-9._-]*) die 'RELEASE_TAG must be a release tag such as v1.0.1.' ;;
esac

base="https://github.com/$GITHUB_REPO/releases/download/$RELEASE_TAG"
echo "Downloading $GITHUB_REPO $RELEASE_TAG ..."
for asset in phicomm-s7d-linux-arm64 phicomm-s7d.init openwrt-firewall-setup.sh; do
    download "$base/$asset" "$workdir/$asset"
    download "$base/$asset.sha256sum" "$workdir/$asset.sha256sum"
done
# Each asset has its own checksum file; verify only the expected filename.
for asset in phicomm-s7d-linux-arm64 phicomm-s7d.init openwrt-firewall-setup.sh; do
    expected=$(awk -v name="$asset" '$2 == name || $2 == "*" name {print $1}' "$workdir/$asset.sha256sum")
    case "$expected" in
        ''|*[!0-9a-fA-F]*) die "Missing or invalid checksum for $asset." ;;
    esac
    [ "${#expected}" = 64 ] || die "Invalid checksum length for $asset."
    actual=$(sha256sum "$workdir/$asset")
    actual=${actual%% *}
    expected=$(printf '%s' "$expected" | tr 'A-F' 'a-f')
    [ "$actual" = "$expected" ] || die "Checksum mismatch: $asset."
done
chmod 0755 "$workdir/phicomm-s7d-linux-arm64"
ash -n "$workdir/phicomm-s7d.init"
ash -n "$workdir/openwrt-firewall-setup.sh"
"$workdir/phicomm-s7d-linux-arm64" --self-test

# Prepare and validate configuration without asking further questions.
if [ "$keep_config" = y ]; then
    cp -p "$config" "$workdir/config.json"
    "$workdir/phicomm-s7d-linux-arm64" --check-config --config "$workdir/config.json" ||
        die "Existing configuration is invalid; fix $config and rerun."
    echo "Keeping $config"
else
    printf '%s\n' "$tcp_listen" "$http_listen" "$data_file" "$height" "$coef_set" |
        "$workdir/phicomm-s7d-linux-arm64" --configure --config "$workdir/config.json" >/dev/null
fi

mkdir -p /usr/bin /etc/init.d /etc/phicom-s7
[ ! -f "$binary" ] || cp -p "$binary" "$workdir/previous-binary"
[ ! -f "$service" ] || cp -p "$service" "$workdir/previous-service"
[ ! -f "$config" ] || cp -p "$config" "$workdir/previous-config"
# Keep a recoverable firewall backup outside the installer's temporary directory.
pending_firewall=$(uci changes firewall)
[ -z "$pending_firewall" ] || die 'Firewall changes appeared during setup; commit or revert them and retry.'
firewall_backup="/tmp/firewall-phicomm-s7-$(date +%Y%m%d-%H%M%S)-$$.backup"
uci export firewall > "$firewall_backup"
if [ -x "$service" ]; then
    if "$service" running >/dev/null 2>&1; then was_running=1; fi
    if "$service" enabled >/dev/null 2>&1; then was_enabled=1; fi
fi
cp "$workdir/phicomm-s7d-linux-arm64" "$binary_stage"
cp "$workdir/phicomm-s7d.init" "$service_stage"
chmod 0755 "$binary_stage" "$service_stage"
rollback_needed=1
if [ -x "$service" ]; then "$service" stop; fi
mv -f "$binary_stage" "$binary"
mv -f "$service_stage" "$service"
if [ "$keep_config" = n ]; then
    cp "$workdir/config.json" "$config_stage"
    chmod 0600 "$config_stage"
    config_changed=1
    mv -f "$config_stage" "$config"
fi
"$service" enable

# Keep the binary, service, configuration/default data and boot symlinks.
touch /etc/sysupgrade.conf
for path in /usr/bin/phicomm-s7d /etc/init.d/phicomm-s7d /etc/phicom-s7/ \
    /etc/rc.d/S95phicomm-s7d /etc/rc.d/K10phicomm-s7d; do
    grep -Fqx "$path" /etc/sysupgrade.conf || printf '\n%s\n' "$path" >> /etc/sysupgrade.conf
done

"$service" start
attempt=0
while ! "$service" running >/dev/null 2>&1; do
    attempt=$((attempt + 1))
    [ "$attempt" -lt 5 ] || die 'Service did not start. Check logread -e phicomm-s7d.'
    sleep 1
done
firewall_changed=1
FIREWALL_ASSUME_YES=1 \
SERVER_IP="$server_ip" SCALE_IP="$scale_ip" CLOUD_IP="$cloud_ip" \
S7_PORT="$cloud_port" LOCAL_PORT="$tcp_port" SCALE_MAC="$scale_mac" SRC_ZONE="$source_zone" \
DEST_ZONE="$dest_zone" FIREWALL_BACKUP="$firewall_backup" \
ash "$workdir/openwrt-firewall-setup.sh"
rollback_needed=0
echo "Installed $RELEASE_TAG; service enabled and started."
echo 'Sysupgrade preservation entries added to /etc/sysupgrade.conf.'
echo 'For a custom data_file outside /etc/phicom-s7/, arrange its backup separately.'
echo "S7 firewall rules installed. Firewall backup: $firewall_backup"
echo "Dashboard: http://$server_ip:$http_port/"
