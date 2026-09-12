#!/usr/bin/env bash
# Publisher only: sign the reviewed deployment stage, not just its binary.
# The signing key must remain outside the source repository and release assets.
set -Eeuo pipefail
umask 077
[[ $# == 4 ]] || { echo 'Usage: package-managed-update.sh <reviewed-stage> <new-output-dir> <private-key> <public-key>' >&2; exit 2; }
stage=$(realpath -e "$1")
output=$(realpath -m "$2")
private_key=$(realpath -e "$3")
public_key=$(realpath -e "$4")
version=$(jq -er '.version' "$stage/managed-release.json")
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+-Bud\.[1-9][0-9]*$ ]]
jq -e '.status=="accepted" and .source_repo=="https://github.com/Bud668/sub2api"' "$stage/managed-release.json" >/dev/null
for file in sub2api prepare-host.sh deploy-reviewed-primary.sh ARTIFACT-SHA256SUMS; do [[ -f "$stage/$file" && ! -L "$stage/$file" ]]; done
[[ $(sha256sum "$stage/sub2api" | cut -d ' ' -f1) == "$(jq -r .binary_sha256 "$stage/managed-release.json")" ]]
(cd "$stage"; sha256sum -c ARTIFACT-SHA256SUMS)
mkdir "$output" # No overwriting published candidates.
filename="sub2api_${version}_linux_amd64.update.tar.gz"
python3 - "$stage" "$output/$filename" <<'PY'
import pathlib, re, sys, tarfile
stage = pathlib.Path(sys.argv[1])
with tarfile.open(sys.argv[2], 'w:gz') as bundle:
    for path in sorted(stage.iterdir()):
        if path.is_symlink() or not path.is_file() or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]{0,127}', path.name):
            raise ValueError('Stage must contain only flat, regular release files')
        bundle.add(path, arcname=path.name, recursive=False)
PY
openssl pkeyutl -sign -inkey "$private_key" -rawin -in "$output/$filename" -out "$output/$filename.sig"
openssl pkeyutl -verify -pubin -inkey "$public_key" -rawin -in "$output/$filename" -sigfile "$output/$filename.sig"
(cd "$output"; sha256sum "$filename" "$filename.sig" > checksums.txt)
echo "Signed update bundle: $output/$filename"
