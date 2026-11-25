#!/usr/bin/env bash
# Make a one-time setup code for the lab's login gate (CONTRACT.md, "Login
# gate"). Prints the code, which goes to the owner only (as the link
# https://<domain>/_lab/setup?code=<code>), and its SHA-256, which is all
# the server ever learns:
#   gh workflow run server.yml --ref main -f action=setup-code -f code_sha256=<sha256>
# (action=reset-password replaces a password that is already set). The code
# works once, for an hour after the workflow stored it.
set -euo pipefail

alphabet=0123456789ABCDEFGHJKMNPQRSTVWXYZ # Crockford's base32
code=""
# 16 characters, 5 random bits each: 256 is a multiple of 32, so % 32 is fair.
for b in $(od -An -tu1 -N16 /dev/urandom); do
  code+="${alphabet:$((b % 32)):1}"
done
echo "code: ${code:0:4}-${code:4:4}-${code:8:4}-${code:12:4}"
echo "sha256: $(printf '%s' "$code" | sha256sum | cut -d' ' -f1)"
