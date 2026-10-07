#!/bin/ash

# Configure DNAT + SNAT rules for a Phicomm S7 and phicomm-s7d.
# Run interactively, or set FIREWALL_ASSUME_YES=1 plus SERVER_IP, SCALE_IP,
# CLOUD_IP, S7_PORT, LOCAL_PORT, SCALE_MAC, SRC_ZONE and DEST_ZONE.

set -eu
umask 077

die() {
    echo "Error: $*" >&2
    exit 1
}

ask() {
    printf '%s [%s]: ' "$1" "$2" >&2
    IFS= read -r reply || die 'Input ended; firewall setup cancelled.'
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

command -v uci >/dev/null 2>&1 || die 'uci was not found; run this script on OpenWrt.'
[ -x /etc/init.d/firewall ] || die 'The OpenWrt firewall service was not found.'
pending=$(uci changes firewall) || die 'Cannot read firewall configuration.'
[ -z "$pending" ] || die 'Commit or revert pending firewall changes before continuing.'

server_default=$(uci -q get firewall.hijack_phicomm_s7_dnat.dest_ip || :)
scale_default=$(uci -q get firewall.hijack_phicomm_s7_dnat.src_ip || :)
cloud_default=$(uci -q get firewall.hijack_phicomm_s7_dnat.src_dip || :)
port_default=$(uci -q get firewall.hijack_phicomm_s7_dnat.src_dport || :)
mac_default=$(uci -q get firewall.hijack_phicomm_s7_dnat.src_mac || :)
source_default=$(uci -q get firewall.hijack_phicomm_s7_dnat.src || :)
dest_default=$(uci -q get firewall.hijack_phicomm_s7_dnat.dest || :)

if [ "${FIREWALL_ASSUME_YES:-}" = 1 ]; then
    server_ip=${SERVER_IP:-${server_default:-192.168.1.1}}
    scale_ip=${SCALE_IP:-${scale_default:-192.168.1.2}}
    cloud_ip=${CLOUD_IP:-${cloud_default:-106.14.93.199}}
    s7_port=${S7_PORT:-${port_default:-30101}}
    local_port=${LOCAL_PORT:-$s7_port}
    scale_mac=${SCALE_MAC:-}
    source_zone=${SRC_ZONE:-${source_default:-lan}}
    dest_zone=${DEST_ZONE:-${dest_default:-lan}}
else
    server_ip=$(ask 'Local phicomm-s7d IPv4 address' "${server_default:-192.168.1.1}")
    scale_ip=$(ask 'Phicomm S7 IPv4 address' "${scale_default:-192.168.1.2}")
    cloud_ip=$(ask 'Original cloud IPv4 address' "${cloud_default:-106.14.93.199}")
    s7_port=$(ask 'S7 TCP port' "${port_default:-30101}")
    local_port=$(ask 'Local phicomm-s7d TCP port' "$s7_port")
    scale_mac=$(ask 'S7 MAC (optional; - clears an existing MAC)' "$mac_default")
    source_zone=$(ask 'S7 source firewall zone' "${source_default:-lan}")
    dest_zone=$(ask 'Server destination firewall zone' "${dest_default:-lan}")
fi
[ "$scale_mac" != - ] || scale_mac=

for address in "$server_ip" "$scale_ip" "$cloud_ip"; do
    valid_ipv4 "$address" || die "Invalid IPv4 address: $address"
done
[ "$server_ip" != "$scale_ip" ] || die 'The server and scale must have different addresses.'
valid_port "$s7_port" || die 'S7 port must be between 1 and 65535.'
valid_port "$local_port" || die 'Local server port must be between 1 and 65535.'
if [ -n "$scale_mac" ]; then
    printf '%s\n' "$scale_mac" | grep -Eq '^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$' || die 'Invalid MAC address.'
fi
for zone in "$source_zone" "$dest_zone"; do
    case "$zone" in ''|*[!A-Za-z0-9_-]*) die "Invalid firewall zone: $zone" ;; esac
done

echo
echo "S7:          $scale_ip${scale_mac:+ ($scale_mac)}"
echo "Cloud:       $cloud_ip:$s7_port"
echo "Local server: $server_ip:$local_port"
echo "Zones:       $source_zone -> $dest_zone"
echo "NAT:         DNAT plus hairpin SNAT"
if [ "${FIREWALL_ASSUME_YES:-}" != 1 ]; then
    confirm=$(ask 'Apply these firewall changes? (y/n)' n)
    case "$confirm" in y|Y|yes|YES) ;; *) echo 'Cancelled; no changes were made.'; exit 0 ;; esac
fi

backup=${FIREWALL_BACKUP:-/tmp/firewall-phicomm-s7-$(date +%Y%m%d-%H%M%S).backup}
case "$backup" in /?*) ;; *) die 'FIREWALL_BACKUP must be an absolute path.' ;; esac
uci export firewall > "$backup" || die "Could not back up firewall configuration to $backup."

restore() {
    uci revert firewall || :
    uci import firewall < "$backup" && uci commit firewall && /etc/init.d/firewall reload
}

if ! {
    uci -q delete firewall.hijack_phicomm_s7_dnat || :
    uci set firewall.hijack_phicomm_s7_dnat=redirect
    uci set firewall.hijack_phicomm_s7_dnat.name=Hijack-Phicomm-s7
    uci set firewall.hijack_phicomm_s7_dnat.target=DNAT
    uci set firewall.hijack_phicomm_s7_dnat.family=ipv4
    uci set firewall.hijack_phicomm_s7_dnat.proto=tcp
    uci set "firewall.hijack_phicomm_s7_dnat.src=$source_zone"
    uci set "firewall.hijack_phicomm_s7_dnat.dest=$dest_zone"
    uci set "firewall.hijack_phicomm_s7_dnat.src_ip=$scale_ip"
    uci set "firewall.hijack_phicomm_s7_dnat.src_dip=$cloud_ip"
    uci set "firewall.hijack_phicomm_s7_dnat.src_dport=$s7_port"
    uci set "firewall.hijack_phicomm_s7_dnat.dest_ip=$server_ip"
    uci set "firewall.hijack_phicomm_s7_dnat.dest_port=$local_port"
    if [ -n "$scale_mac" ]; then uci set "firewall.hijack_phicomm_s7_dnat.src_mac=$scale_mac"; fi
    uci -q delete firewall.hijack_phicomm_s7_snat || :
    uci set firewall.hijack_phicomm_s7_snat=nat
    uci set firewall.hijack_phicomm_s7_snat.name=Hijack-Phicomm-s7
    uci set firewall.hijack_phicomm_s7_snat.target=SNAT
    uci set firewall.hijack_phicomm_s7_snat.family=ipv4
    uci set firewall.hijack_phicomm_s7_snat.proto=tcp
    uci set "firewall.hijack_phicomm_s7_snat.src=$source_zone"
    uci set "firewall.hijack_phicomm_s7_snat.src_ip=$scale_ip"
    uci set "firewall.hijack_phicomm_s7_snat.dest_ip=$server_ip"
    uci set "firewall.hijack_phicomm_s7_snat.dest_port=$local_port"
    uci set "firewall.hijack_phicomm_s7_snat.snat_ip=$cloud_ip"
    if command -v fw4 >/dev/null 2>&1; then fw4 check; fi
    uci commit firewall
    /etc/init.d/firewall reload
}; then
    echo "Firewall setup failed; restoring backup: $backup" >&2
    restore || echo "Automatic firewall restore failed. Restore with: uci import firewall < $backup && uci commit firewall && /etc/init.d/firewall reload" >&2
    exit 1
fi

echo "Firewall rules installed successfully. Backup: $backup"
