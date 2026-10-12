#!/bin/sh
# Regenerates docs/wame-tui.png from mocked data. Needs vhs and ffmpeg.
# VHS records raw frames here and the last one is padded with ffmpeg directly, because VHS
# silently produces nothing when its own ffmpeg step fails (seen with ffmpeg 9).
set -eu
cd "$(dirname "$0")"
go build -o wame-demo .
rm -rf frames
vhs -q demo.tape
last=$(ls frames/frame-text-*.png | tail -n 1)
ffmpeg -hide_banner -loglevel error -y -i "$last" -vf "pad=iw+56:ih+56:28:28:color=0x1e1e2e" ../docs/wame-tui.png
rm -rf frames wame-demo demo.db demo.db-shm demo.db-wal
echo "wrote docs/wame-tui.png"
