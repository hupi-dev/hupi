#!/bin/bash
# Saves the Windows VM's screen as a PNG, through QEMU's monitor: vmshot.sh OUT.png
# (HUPI_WIN_CONTAINER picks the container; default hupi-win).
docker exec "${HUPI_WIN_CONTAINER:-hupi-win}" sh -c 'echo "screendump /run/shm/s.ppm" | nc -U -q1 /run/shm/monitor.sock' >/dev/null 2>&1
docker exec "${HUPI_WIN_CONTAINER:-hupi-win}" cat /run/shm/s.ppm | python3 -c "import sys,io; from PIL import Image; Image.open(io.BytesIO(sys.stdin.buffer.read())).save(sys.argv[1])" "$1"
