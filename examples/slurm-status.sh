#!/bin/sh
# Status hook for a Slurm job on slurm-gcp, where Slurm node names are the
# VM names. Set SLURM_JOB to the job id.
#
#   lockstep run --status-hook examples/slurm-status.sh ...
#
# If squeue fails this exits nonzero, and lockstep does nothing that
# round rather than guess the job is down.
set -eu
state=$(squeue -h -j "$SLURM_JOB" -o %T)
if [ "$state" != "RUNNING" ]; then
	echo down
	exit 0
fi
echo running
scontrol show hostnames "$(squeue -h -j "$SLURM_JOB" -o %N)"
