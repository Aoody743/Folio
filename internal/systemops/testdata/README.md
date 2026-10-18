# Kernel JSON provenance

`nft-1.1.3-debian13.json` is unchanged output from a real Debian 13 amd64 QEMU
guest on 2026-10-13. QA created an isolated `powerlinux-systemops-fixture` table,
read `nft --json list table`, then deleted the table and confirmed it was absent.
nft 1.1.3 and dependencies came from signed Debian APT metadata with package
SHA256 verification, privately extracted without enabling a global nft service.

The test normalizes only the temporary table/comment names. Actual kernel
expression JSON remains unchanged. This catches nft's direct state-list format
and is evidence for parser compatibility; it does not establish an end-to-end
Folio firewall installation/confirmation/reboot test.
