#!/bin/sh
set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get -q update
apt-get -q install -y --no-install-recommends docker.io docker-buildx docker-compose-v2 make squid iptables acl ca-certificates
install -d /etc/docker
printf '%s\n' '{"features":{"containerd-snapshotter":true}}' >/etc/docker/daemon.json
systemctl restart docker
docker info --format '{{json .DriverStatus}}' | grep -q io.containerd.snapshotter
useradd --create-home --shell /bin/bash developer
cat >/etc/squid/squid.conf <<'CONF'
http_port 127.0.0.1:3128
acl approved dstdomain .buf.build .bufusercontent.com proxy.golang.org sum.golang.org storage.googleapis.com
acl connect method CONNECT
acl tls port 443
http_access allow approved connect tls
http_access deny all
access_log none
CONF
systemctl restart squid
iptables -N KANZ_DEV_ACQUISITION
iptables -A KANZ_DEV_ACQUISITION -d 127.0.0.1 -p tcp --dport 3128 -j ACCEPT
iptables -A KANZ_DEV_ACQUISITION -j REJECT
iptables -I OUTPUT -m owner --uid-owner developer -j KANZ_DEV_ACQUISITION
ip6tables -I OUTPUT -m owner --uid-owner developer -j REJECT
install -d /work/repo /opt/proof
