# packaging

`package-manager.<format>` is installed by the deb, rpm and apk packages
(see `nfpms:` in `.goreleaser.yml`) as `/usr/share/fsend/package-manager`.
Its one line names the package format. `fsend --update` and
`fsend --uninstall` read it to leave a packaged binary to the package
manager instead of replacing or deleting it behind its back.
