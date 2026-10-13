#!/usr/bin/env bash
# Boot a verified Ubuntu cloud image/NoCloud seed prepared outside the checkout.
set -euo pipefail
qa_ubuntu_dir="${POWERLINUX_QA_UBUNTU_DIR:-/workspace/tools/qa-ubuntu/guest}"
qa_qemu="${POWERLINUX_QA_QEMU:-/workspace/tools/qa-vm/root/usr/bin/qemu-system-x86_64}"
qa_qemu_data="${POWERLINUX_QA_QEMU_DATA:-/workspace/tools/qa-vm/root/usr/share/qemu}"
qa_bios="${POWERLINUX_QA_BIOS:-/workspace/tools/qa-vm/root/usr/share/seabios/bios-256k.bin}"
: "${POWERLINUX_QA_QEMU_LIB:=/workspace/tools/qa-vm/root/usr/lib/x86_64-linux-gnu}"
export LD_LIBRARY_PATH="$POWERLINUX_QA_QEMU_LIB${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
for qa_required in "$qa_ubuntu_dir/ubuntu-overlay.qcow2" "$qa_ubuntu_dir/seed.iso" "$qa_qemu" "$qa_bios"; do
  test -f "$qa_required" || { printf '%s\n' "Required verified VM artifact is missing: $qa_required" >&2; exit 1; }
done
exec "$qa_qemu" -L "$qa_qemu_data" -bios "$qa_bios" -machine pc -accel tcg,thread=multi -smp 2 -m 1024 \
  -display none -vga none -monitor none -serial "file:$qa_ubuntu_dir/serial.log" \
  -drive "file=$qa_ubuntu_dir/ubuntu-overlay.qcow2,format=qcow2,if=virtio" \
  -drive "file=$qa_ubuntu_dir/seed.iso,format=raw,media=cdrom,readonly=on" \
  -netdev user,id=qa-net,hostfwd=tcp:127.0.0.1:19023-:22 \
  -device virtio-net-pci,netdev=qa-net,mac=52:54:00:12:34:58,romfile=
