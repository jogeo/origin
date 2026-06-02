#!/usr/bin/env bash
STARTTIME=$(date +%s)
source "$(dirname "${BASH_SOURCE}")/lib/init.sh"

OUTPUT_DIR="${1:-test/extended/_catalog}"

echo "Generating test catalog into ${OUTPUT_DIR}..."
go run -mod vendor ./hack/testcatalog -input=test/extended -output="${OUTPUT_DIR}"

ret=$?; ENDTIME=$(date +%s); echo "$0 took $(($ENDTIME - $STARTTIME)) seconds"; exit "$ret"
