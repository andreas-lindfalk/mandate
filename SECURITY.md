# Security

pgmandate is early software. It has had no external security review. Read [docs/threat-model.md](docs/threat-model.md) before relying on it: it says what the design protects, against whom, and what it does not.

## Reporting a vulnerability

Please do not open a public issue for a vulnerability. Use GitHub's private reporting on this repository (Security, then "Report a vulnerability"). You will get an answer within a week. Say what you found, how to reproduce it, and what you think the impact is.

Things that are known and documented are not vulnerabilities, but better ideas for them are welcome as ordinary issues: the trust placed in the application's database role, connector secrets and notifier addresses stored in the clear, and the pattern-based steering-text tripwire.
