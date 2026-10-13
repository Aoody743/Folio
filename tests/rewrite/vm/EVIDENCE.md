# Clean VM verification evidence

Status: actual `0.1.0-dev.3` production installer lifecycle **passed** in the real Debian13 amd64 QEMU guest. Authenticated SSH with pinned host key verifies PID1=systemd, virtualization=qemu, kernel6.12.107+deb13-amd64, ssh.service=active and qa-network.service=active.

Verified archive SHA256: `68c7dba055ca0106fd5ca380e61adebf5b4748df0f5c5c35545c06d59427abc6`.

The unmodified `test_install.py` completed with exit0 against actual package: clean PowerLinux state install, ordinary-user service, exact version/ready health, repeated installer, protected global-file hashes unchanged, real administrator/notes API through an SSH tunnel, service restart preserving the note, and uninstall retaining the database while removing the service/current link. This is real systemd evidence, not container or mocked service evidence.

Evidence captured outside checkout:

- `/workspace/tools/qa-vm/guest/installer-evidence.json`
- `/workspace/powerlinux/test-artifacts/vm-dev3.log`

The first dev.1 installer caller hit its150s SSH deadline during the initial cold guest trial. This is retained as a failed caller result. Subsequent diagnosis found the real ordinary-user service running with exact healthy JSON and no pending transaction; repeating the installer exited0 in24.88s, with its health test succeeding on the first attempt. Only the owned trial unit, directories and test user were reset. Fresh dev.3 then passed the original150s installation deadline and full lifecycle driver; the timeout was not increased to force a pass.

Different-version upgrades/failed-health rollback, guest reboot, Ubuntu and native arm64 remain **unverified**. dev.4 changes only UI source labels; its browser/package checks are separate from the dev.3 VM lifecycle evidence.

Environment: Debian13 cloud execution host, no `/dev/kvm`, no preinstalled QEMU. QEMU10.0.13 (Debian `1:10.0.13+ds-0+deb13u1`) and required libraries were downloaded through APT, with Debian InRelease signature and package hashes checked normally, then extracted under `/workspace/tools/qa-vm`. No host package installation or global settings changed.

Official Debian/Ubuntu cloud-image checksum URLs returned HTTP403. A separate route prepares a clean Debian13 disk:

- Official Docker `debian:trixie-slim`, pinned digest `sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f`, downloaded with layer checksum verification.
- 71 additional Debian packages resolved against the builder's dpkg status and downloaded on the host from signed `https://deb.debian.org/debian trixie main` metadata; the builder receives already verified packages and installs offline. TLS, signatures and package hash verification were retained. Docker's own outbound APT initially failed TLS certificate verification; it was not bypassed.
- Debian kernel `6.12.107+deb13-amd64`, systemd `257.13-1~deb13u1`, OpenSSH server `1:10.0p1-7+deb13u4`, ext4 disk2GiB.
- Dedicated SSH keys; known host key copied directly from the trusted builder and checked by SSH. Private keys remain outside the repository and are not printed or committed.
- Guest runs via QEMU TCG, 2 virtual CPUs, 2GiB RAM. SSH forwarding is bound only to localhost port19022. No shared host directories are mounted into the guest.

TCG timing is not hardware-backed Linux performance evidence. This custom guest can prove systemd installer behavior on Debian13 amd64, not Ubuntu, arm64, public cloud image defaults or a full platform support matrix.
