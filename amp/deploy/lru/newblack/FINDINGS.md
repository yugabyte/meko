# newblack LRU findings

What the workload has shown on amp, newest first. One dated entry per finding,
with the evidence and the issue it became.

## 2026-09-09 — first run on meko_dev_eks

Deployed against pool `lru-newblack`, cluster `newblack`, one 4 vCPU / 32Gi
endpoint. The schema converts and loads on amp's Postgres 18 without a manual
step: 51 tables, 174 indexes, seed row counts identical to a local Postgres run.

Steady at 299.7 of a 300 TPS target after seven minutes, 0 errors and 0
transaction retries, RO/RW lanes splitting 169/131 as the mix weights predict.
On the endpoint that is ~170 commits/s, ~818 tuples fetched/s and ~26 tuples
inserted/s, with a buffer hit ratio of 0.99999. Nothing to report on the product
yet: this is the baseline the next weeks are measured against.

Three things needed fixing in the LRU itself, none of them amp's doing: the
control plane's 8 GiB per vCPU floor, the checker image having no curl for the
hourly sync to exec, and the driver printing mean latency only at cycle end.
