#!/usr/bin/env bash
# Boot a real disposable Debian systemd guest; this is not a Docker runtime.
set -euo pipefail
: "${POWERLINUX_QA_VM_DIR:?path containing verified vmlinuz, initrd.img and debian13.ext4}"
: "${POWERLINUX_QEMU:=qemu-system-x86_64}"
: "${POWERLINUX_QEMU_DATA:=/usr/share/qemu}"
: "${POWERLINUX_QEMU_BIOS:=/usr/share/seabios/bios-256k.bin}"
exec "$POWERLINUX_QEMU" -accel tcg,thread=multi -smp 2 -m 2048 \
  -display none -vga none -monitor none -L "$POWERLINUX_QEMU_DATA" -bios "$POWERLINUX_QEMU_BIOS" \
  -kernel "$POWERLINUX_QA_VM_DIR/vmlinuz" -initrd "$POWERLINUX_QA_VM_DIR/initrd.img" \
  -append 'root=/dev/vda rw console=ttyS0 net.ifnames=0 biosdevname=0' \
  -drive "file=$POWERLINUX_QA_VM_DIR/debian13.ext4,format=raw,if=virtio" \
  -netdev user,id=qa,hostfwd=tcp:127.0.0.1:19022-:22 \
  -device virtio-net-pci,netdev=qa,romfile= \
  -serial "file:$POWERLINUX_QA_VM_DIR/serial.log"
