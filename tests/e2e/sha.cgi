#!/bin/sh
# Returns the SHA-256 of the uploaded body (upload-direction test).
echo "Content-Type: text/plain"
echo
head -c "${CONTENT_LENGTH:-0}" | sha256sum | cut -d' ' -f1
