# Optional browser applications

Firefox WASM and Chrome headless shell are downloaded only when the corresponding
application is installed. They are not bundled in the panel's default download.

The pinned versions, download URLs and SHA-256 digests are defined in
[`apps.go`](../../../internal/serverbrowser/apps.go). Extraction rejects paths
outside the app directory, links, oversized archives and duplicate files.

Attribution and source availability are described in
[`NOTICE.txt`](../../../internal/serverbrowser/notices/NOTICE.txt), with a copy of
the [Mozilla Public License 2.0](../../../internal/serverbrowser/notices/MPL-2.0.txt).
Both files are installed beside Firefox WASM and available from its launch page.
Chrome's upstream license and notices remain in its managed engine directory.
