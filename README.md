# lockstep

GPU VMs on Compute Engine can't live-migrate. When a host needs
maintenance, the VM on it is stopped. A training job spread over 256 of
these VMs restarts every time any one of them goes, so each node's
maintenance window costs the whole job a restart, plus whatever it hadn't
checkpointed.

lockstep lines those windows up. It watches the job's VMs for upcoming
maintenance and waits until the earliest window is about to start. Then it
stops the job once, cleanly, and starts maintenance on every other node
whose window is coming up soon, using `performMaintenance`. When the job is
down anyway, say after a GPU failure, pending maintenance rides along on
the restart that is already happening. It only sends as many nodes along as
the spares can cover, unless the restart is waiting for nodes anyway.

```
curl -fsSL https://raw.githubusercontent.com/Rakshit-gen/lockstep/main/scripts/install.sh | sh
```

## What Google gives you, and the gap

Taken from the docs linked at the bottom:

- A3 High, A3 Mega, A3 Ultra, A4 and A4X VMs get scheduled maintenance at
  least 90 days apart, with 90 days' notice. Emergency maintenance comes
  with 7 days' notice if emergency notifications are on, a few hours if not.
  A3 Edge is every 30 days with 7 days' notice. Other GPU VMs get
  maintenance about every two weeks and an hour's notice.
- Each VM shows its next window in `resourceStatus.upcomingMaintenance`,
  and in the metadata server at `instance/upcoming-maintenance`. When
  `canReschedule` is true you can start it early with `performMaintenance`.
- Grouped reservations can have Compute Engine run planned maintenance on
  every VM in the group at once. slurm-gcp can keep jobs off nodes with
  maintenance coming up. GKE lets you start pending maintenance on a node
  with a label.

None of these batch emergency maintenance into a stop you were going to
make anyway, or use a failure's restart to clear pending windows. Each of
those is a whole-job restart avoided.

## How it works

`plan.Decide` is a pure function used by both the controller and the
simulator:

- **Job running:** maintenance on idle nodes starts early, since it can't
  hurt the job. If any node in the job has a window, wait until the
  earliest is `--margin` away (15 minutes by default, enough for a final
  checkpoint). Then stop the job and start that node's maintenance. Other
  job nodes due within `--horizon` (14 days) go too, as many as the spares
  cover.
- **Job down:** start maintenance on nodes due within the horizon. If
  enough nodes are up to restart right away, only send as many as the
  spares cover, soonest first. If the restart is waiting for nodes anyway,
  send all of them.

The horizon matters. On A3 a node shows its next 90-day window as soon as
maintenance ends, so without a horizon every stop would maintain every
node.

`lockstep run` is the loop. It talks to your scheduler through three
hooks:

- `--status-hook` prints `running` followed by the job's VM names, or
  `down`. Anything else counts as an error, and lockstep does nothing that
  round instead of guessing.
- `--stop-hook` checkpoints and stops the job. Maintenance on the job's VMs
  starts only after this exits 0.
- `--start-hook` is optional. It gets `LOCKSTEP_NODES`, the VMs that are up
  and staying up.

```
lockstep status --project p --filter 'labels.job = "llama"'
lockstep plan   --project p --filter 'labels.job = "llama"' --size 256 --status-hook examples/slurm-status.sh
lockstep run    --project p --filter 'labels.job = "llama"' --size 256 \
                --status-hook examples/slurm-status.sh --stop-hook examples/slurm-stop.sh
```

`plan` is a dry run: it prints what `run` would do right now. On a VM,
`lockstep agent --hook ./checkpoint.sh` watches `instance/maintenance-event`
and runs the hook when the host is about to take that VM, which on GPU VMs
is about an hour ahead. `examples/lockstep-agent.service` runs it under
systemd.

Credentials come from `LOCKSTEP_TOKEN`, the metadata server when running
on a VM, or `gcloud auth print-access-token`. The VMs need
`compute.instances.list` and `compute.instances.performMaintenance`.
`LOCKSTEP_API` points it at another endpoint; the tests use the fake in
`gce/gcetest`.

## Does it help?

I haven't run this on a real A3 cluster, so every number here comes from a
simulator with an invented world. The maintenance cadence and notice come
from Google's docs. The failure rate comes from the Llama 3 paper: 419
unexpected interruptions in 54 days on 16,384 GPUs, which works out to
0.0038 per 8-GPU node per day. Everything else is an assumption with a flag:
restart time, how long maintenance takes, repair time, checkpoint interval,
and the emergency rate.

The policies:

- **none:** every maintenance crashes the job.
- **agent:** a per-node agent checkpoints before its own node goes. Every
  window still costs a restart.
- **grouped:** the agent plus Google's grouped maintenance, so planned
  windows land on every node at once.
- **lockstep:** the agent plus this tool.
- **both:** grouped plus lockstep.

All of them place the job on the nodes whose maintenance is furthest off,
which leaves the soonest ones as spares. slurm-gcp already does something
like that.

Every number below comes from the simulator's invented world, averaged over 20 seeds of a 365 day run.
Assumed unless the flag is changed: restart 0.5h, maintenance 2h, repair 4h, checkpoint every 1h, 5 minute final checkpoint, emergency events 0.001 per node per day with 7 days notice.
Failures 0.0038 per node per day, from the Llama 3 paper.

#### Goodput by job size, A3 cadence, one spare per 16 nodes

| nodes | spares | none | agent | grouped | lockstep | both | stops agent | stops lockstep |
|---|---|---|---|---|---|---|---|---|
| 16 | 1 | 0.9891 | 0.9925 | 0.9954 | 0.9927 | 0.9952 | 92 | 80 |
| 64 | 4 | 0.9845 | 0.9867 | 0.9884 | 0.9883 | 0.9881 | 136 | 107 |
| 256 | 16 | 0.9594 | 0.9596 | 0.9588 | 0.9600 | 0.9590 | 361 | 355 |
| 1024 | 64 | 0.8513 | 0.8513 | 0.8504 | 0.8511 | 0.8504 | 1399 | 1402 |

#### Spares, 256 nodes, A3 cadence

| spares | none | agent | grouped | lockstep | both |
|---|---|---|---|---|---|
| 0 | 0.5654 | 0.5951 | 0.7722 | 0.8150 | 0.8154 |
| 4 | 0.9366 | 0.9464 | 0.9580 | 0.9552 | 0.9579 |
| 16 | 0.9594 | 0.9596 | 0.9588 | 0.9600 | 0.9590 |
| 64 | 0.9603 | 0.9603 | 0.9594 | 0.9606 | 0.9601 |

#### Machine families, 256 nodes, 16 spares

| preset | period | notice | none | agent | grouped | lockstep | both |
|---|---|---|---|---|---|---|---|
| a3 | 90d | 90d | 0.9594 | 0.9596 | 0.9588 | 0.9600 | 0.9590 |
| a3-edge | 30d | 7d | 0.9488 | 0.9535 | 0.9566 | 0.9565 | 0.9561 |
| gpu | 14d | 1h | 0.5304 | 0.7231 | 0.9527 | 0.7131 | 0.9500 |

#### Horizon, 256 nodes, 16 spares, A3 cadence

| horizon | goodput | stops | maintenance run | started early |
|---|---|---|---|---|
| 1h | 0.9597 | 361 | 1143 | 236 |
| 1d | 0.9597 | 359 | 1154 | 999 |
| 3d | 0.9597 | 357 | 1184 | 1171 |
| 7d | 0.9598 | 356 | 1250 | 1250 |
| 14d | 0.9600 | 355 | 1358 | 1358 |
| 30d | 0.9596 | 354 | 1699 | 1699 |

What I take from it:

- **With one spare per 16 nodes, maintenance hardly matters on A3.** At
  256 nodes and up, every policy lands within about a tenth of a point of
  the others. The spares absorb the windows, and failures account for
  nearly all the stops. If you run A3 or A4 with spares, you don't need
  this.
- **It pays when spares are short.** With 256 nodes and no spares,
  lockstep takes goodput from 0.595 (agent) to 0.815, ahead of grouped
  maintenance at 0.772, because it also lines up emergency windows and
  failures. With four spares it is about even with grouped. Running both
  stays within 0.3 points of the better of the two in every row.
- **It can't help without notice.** On GPU VMs with an hour's notice there
  is nothing to batch, and lockstep lands behind the plain agent. There,
  grouped maintenance is the thing to use.
- **The horizon barely moves goodput.** A longer one runs more
  maintenance, though: 1,699 windows a year at 30 days against 1,143 at an
  hour. That's because the next window counts from when maintenance ran.
  Fourteen days is the default.

The simulator runs each policy on the same seeds, and `lockstep sim` takes
every assumption as a flag:

```
lockstep sim --size 256 --spares 0 --restart 1 --maint 4 --emergency-rate 0.005
lockstep bench
```

## Limits

- Not tested against real Compute Engine. The client follows the v1 REST
  docs and is tested against the fake, which accepts `performMaintenance`
  only on pending, reschedulable windows.
- One job per controller. The job either runs on all of its nodes or is
  down; elastic jobs aren't modeled.
- The emergency rate, restart time and maintenance length are guesses. If
  you have real numbers, pass them to `lockstep sim`.

## Sources

- [Manage host events across compute instances](https://docs.cloud.google.com/ai-hypercomputer/docs/manage/host-events), AI Hypercomputer
- [Handle GPU host maintenance events](https://docs.cloud.google.com/compute/docs/gpus/gpu-host-maintenance), Compute Engine
- [Monitor and plan for a host maintenance event](https://docs.cloud.google.com/compute/docs/instances/monitor-plan-host-maintenance-event), Compute Engine
- [Handle disruption on GKE nodes with GPUs and TPUs](https://cloud.google.com/kubernetes-engine/docs/concepts/handle-disruption-gpu-tpu)
- [Cluster Toolkit slurm-gcp](https://github.com/GoogleCloudPlatform/slurm-gcp), maintenance reservations
- [The Llama 3 Herd of Models](https://arxiv.org/abs/2407.21783), section 3.3.4, for the failure rate

## License

MIT
