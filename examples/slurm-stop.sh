#!/bin/sh
# Stop hook for a Slurm job. It signals the training script to write a
# checkpoint (it should catch USR1), gives it CKPT_WAIT seconds, then
# requeues the job. Slurm starts it again on nodes that are up, so no
# start hook is needed.
set -eu
scancel --full --signal=USR1 "$SLURM_JOB"
sleep "${CKPT_WAIT:-300}"
scontrol requeue "$SLURM_JOB"
