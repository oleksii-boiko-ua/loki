#!/bin/sh

docker ps
image="$(docker ps --filter ancestor=jrei/systemd-debian:12 --latest --format {{.ID}})"
echo "Running on container: $image"

dir="."
if [ ! -z "$CI" ]; then
    dir="/drone/src"
fi
echo "Running on directory: $dir"

cat <<EOF | docker exec --interactive $image sh
    # Install loki and check it's running
    dpkg -i ${dir}/dist/loki_0.0.0~rc0_amd64.deb
    [ "\$(systemctl is-active loki)" = "active" ] || (echo "loki is inactive" && exit 1)

    # Install promtail and check it's running
    dpkg -i ${dir}/dist/promtail_0.0.0~rc0_amd64.deb
    [ "\$(systemctl is-active promtail)" = "active" ] || (echo "promtail is inactive" && exit 1)

    # Write some logs
    mkdir -p /var/log/
    echo "blablabla" > /var/log/test.log

    # Install logcli
    dpkg -i ${dir}/dist/logcli_0.0.0~rc0_amd64.deb

    # Check that there are labels
    sleep 5
    labels_found=\$(logcli labels)
    echo "Found labels: \$labels_found"
    [ "\$labels_found" != "" ] || (echo "no logs found with logcli" && exit 1)
EOF