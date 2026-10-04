#!/bin/sh
# /agyn/bin/node, delivered by Dockerfile.reporting-init: init scripts and
# terminal commands run Node by this path in workspace images that carry none.
# node.bin is glibc-linked and finds the C++ runtime it was built against in
# /agyn/lib; a library path the workspace already set is kept after it.
export LD_LIBRARY_PATH="/agyn/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
exec /agyn/bin/node.bin "$@"
