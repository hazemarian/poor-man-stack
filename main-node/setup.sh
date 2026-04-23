#!/bin/bash
set -e
exec "$(dirname "$0")/../bin/setup.sh" manager "$@"
