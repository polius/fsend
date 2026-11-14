# Changelog

Full release notes for every version are on
[GitHub Releases](https://github.com/polius/fsend/releases). This file
lists the changes that need action, newest first. See
[Version compatibility](docs/compatibility.md) for which versions work
together.

## 1.16.0

- **Protocol 3: update both ends.** Sender and receiver both need 1.16 or
  later. An older peer can't find a 1.16 sender's code (`E002`).
- **Update self-hosted servers with the clients.** A 1.16 server refuses
  clients older than 1.16 (HTTP 426, `E060`), and 1.16 clients can't use
  an older server's relay.

## 1.10.0

- **Symlink entries are rejected on receive.** They closed a path
  traversal. Senders already sent the targets of symlinks as real files,
  so normal transfers are unaffected.

## 1.6.0

- **Protocol 2: update both ends.** Sender and receiver both need 1.6 or
  later. Mixing with 1.0 – 1.5 fails with `E034`.
