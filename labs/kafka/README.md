# Kafka LAB on devk8s

A 3-broker Apache Kafka 4.0 cluster in **KRaft mode** (no ZooKeeper) running in namespace `kafka`
on the `devk8s-admin@devk8s` context. Every command in this guide was run against this cluster.

| Thing | Value |
|---|---|
| Brokers | `kafka-0`, `kafka-1`, `kafka-2` (StatefulSet, one per worker node) |
| Each node is | broker **and** KRaft controller (node ids 0, 1, 2) |
| Storage | 5 Gi `ceph-block` PVC per broker (`data-kafka-N`) |
| Client port | `9092` (PLAINTEXT, no auth, in-cluster only) |
| Bootstrap | `kafka-0.kafka-headless.kafka.svc.cluster.local:9092` |
| CLI pod | `kafka-client` (has every Kafka CLI tool; env var `$BS` holds all 3 brokers) |
| Auto topic creation | **off** (you create topics on purpose) |
| Defaults | RF=3, `min.insync.replicas=2`, 3 partitions, retention 7 days |

Manifest: [`kafka.yaml`](kafka.yaml). Deploy: `kubectl --context devk8s-admin@devk8s apply -f kafka.yaml`.

---

## 0. Ten-minute mental model

* **Topic** = a named log. Split into **partitions** (the unit of parallelism and ordering).
* **Ordering is guaranteed only inside one partition.** Messages with the same **key** always go to the same partition.
* Each partition has **replicas** on different brokers: one **leader** (all reads/writes) and followers.
* **ISR** (in-sync replicas) = replicas that are caught up with the leader. This is the number to watch.
* **Producer** `acks=all` + topic `min.insync.replicas=2` means: "a write succeeds only when at least 2 copies have it".
  With RF=3 you can lose **1** broker with no downtime and no data loss.
* **Consumer group** = consumers sharing a workload. Each partition is read by exactly one member of a group.
  Position is stored as an **offset** per (group, partition). **Lag** = latest offset − committed offset.
* **Retention** is about **time/size, not about whether someone read the message**. Kafka is a log, not a queue.
* **KRaft controllers** store cluster metadata (topics, leaders). They need a **majority (2 of 3)** alive.

---

## 1. Connecting

Everything is in-cluster, so you run the CLI from the `kafka-client` pod.

```bash
alias k='kubectl --context devk8s-admin@devk8s -n kafka'
k get pods -o wide                       # 3 brokers + kafka-client, brokers on different nodes
k exec -it kafka-client -- bash          # open a shell in the CLI pod
```

Inside the pod:

```bash
export PATH=$PATH:/opt/kafka/bin
echo $BS                                 # all three brokers, comma separated
kafka-topics.sh --bootstrap-server $BS --list
```

`--bootstrap-server` is only the *first contact*. The client asks any one broker for the cluster
metadata, then talks directly to the partition leaders. List more than one broker so you can still
connect when one is down (that is why `$BS` has all three).

Quick health check (who is the controller leader, are all 3 voters healthy):

```bash
kafka-metadata-quorum.sh --bootstrap-server $BS describe --status
kafka-metadata-quorum.sh --bootstrap-server $BS describe --replication   # per-node lag
kafka-broker-api-versions.sh --bootstrap-server $BS | grep -E "^kafka"   # one line per live broker
```

> Reaching Kafka from your laptop is **not** wired up on purpose: brokers advertise their in-cluster
> DNS names, so `kubectl port-forward` will connect and then fail on the redirect. Ask if you want an
> external listener (NodePort / LoadBalancer) added; it is a good second lab.

---

## 2. Topics

```bash
# Create: 3 partitions, every partition copied on all 3 brokers
kafka-topics.sh --bootstrap-server $BS --create --topic orders --partitions 3 --replication-factor 3

kafka-topics.sh --bootstrap-server $BS --list
kafka-topics.sh --bootstrap-server $BS --describe --topic orders
```

Real output:

```
Topic: orders  PartitionCount: 3  ReplicationFactor: 3  Configs: min.insync.replicas=2,segment.bytes=104857600
  Partition: 0  Leader: 1  Replicas: 1,2,0  Isr: 1,2,0
  Partition: 1  Leader: 2  Replicas: 2,0,1  Isr: 2,0,1
  Partition: 2  Leader: 0  Replicas: 0,1,2  Isr: 0,1,2
```

How to read it: `Leader` = broker id serving the partition; `Replicas` = where copies live (first is the
"preferred" leader); `Isr` = copies that are in sync. **Healthy = `Isr` equals `Replicas`.**

```bash
# Add partitions later (you can only INCREASE; keyed messages will re-map to new partitions)
kafka-topics.sh --bootstrap-server $BS --alter --topic orders --partitions 6

# Delete
kafka-topics.sh --bootstrap-server $BS --delete --topic orders
```

Rule of thumb for RF: **3 in production-like setups**. RF cannot exceed the broker count (here 3).

---

## 3. Produce and consume

```bash
# Terminal A: consumer (leave running)
kafka-console-consumer.sh --bootstrap-server $BS --topic orders --from-beginning

# Terminal B: producer (type lines, Ctrl-D to finish)
kafka-console-producer.sh --bootstrap-server $BS --topic orders
> order-1
> order-2
```

> Non-interactive from your Mac: `printf 'a\nb\n' | kubectl --context devk8s-admin@devk8s -n kafka exec -i kafka-client -- /opt/kafka/bin/kafka-console-producer.sh --bootstrap-server $BS --topic orders` (use the literal broker list, `$BS` is not expanded locally).

Keys and metadata (this shows partitioning and ordering):

```bash
# Producer with keys:  key:value
kafka-console-producer.sh --bootstrap-server $BS --topic orders \
  --property parse.key=true --property key.separator=:
> cust-A:order-1
> cust-B:order-2
> cust-A:order-3

# Consumer printing where each message landed
kafka-console-consumer.sh --bootstrap-server $BS --topic orders --from-beginning \
  --property print.key=true --property print.partition=true --property print.offset=true \
  --property print.timestamp=true
```

Observe: all `cust-A` records are in the **same partition, in order**. Records across partitions can
interleave in any order, so a plain `--from-beginning` read of 3 partitions is *not* in send order.

Safest producer settings (use when you care about not losing data):

```bash
kafka-console-producer.sh --bootstrap-server $BS --topic orders \
  --producer-property acks=all --producer-property enable.idempotence=true
```

Read one partition from a specific offset:

```bash
kafka-console-consumer.sh --bootstrap-server $BS --topic orders --partition 0 --offset 2 --max-messages 5
```

> `--timeout-ms N` makes the consumer stop after N ms of silence. It prints a `TimeoutException` when it
> does; that is normal, not an error in Kafka.

---

## 4. Consumer groups and lag

```bash
# Two consumers in the same group split the partitions. Run in two terminals:
kafka-console-consumer.sh --bootstrap-server $BS --topic orders --group billing

kafka-consumer-groups.sh --bootstrap-server $BS --list
kafka-consumer-groups.sh --bootstrap-server $BS --describe --group billing
```

Columns: `CURRENT-OFFSET` (where the group is), `LOG-END-OFFSET` (latest), `LAG` (the difference),
`CONSUMER-ID`. **Growing LAG = consumers can't keep up (or are dead).**
Stop one consumer and watch the other take over its partitions (a *rebalance*).

Replay / skip data by moving the group's offsets (group must be inactive, i.e. consumers stopped):

```bash
kafka-consumer-groups.sh --bootstrap-server $BS --group billing --topic orders \
  --reset-offsets --to-earliest --dry-run          # preview
kafka-consumer-groups.sh --bootstrap-server $BS --group billing --topic orders \
  --reset-offsets --to-earliest --execute          # apply (also: --to-latest, --shift-by -10, --to-datetime ...)
```

---

## 5. Message retention

Kafka deletes **old segment files**, not individual messages, and **never the active (currently written)
segment**. A segment is eligible once it is closed and its newest record is older than `retention.ms`.

Broker defaults in this lab (set in `kafka.yaml`): `retention.hours=168` (7 days),
`segment.bytes=100 MiB`, retention check every 60 s.

Per-topic overrides with `kafka-configs.sh`:

```bash
# See current overrides
kafka-configs.sh --bootstrap-server $BS --entity-type topics --entity-name orders --describe

# Keep 1 day OR 1 GiB per partition, whichever limit is hit first
kafka-configs.sh --bootstrap-server $BS --entity-type topics --entity-name orders --alter \
  --add-config retention.ms=86400000,retention.bytes=1073741824

# Back to broker default
kafka-configs.sh --bootstrap-server $BS --entity-type topics --entity-name orders --alter \
  --delete-config retention.ms,retention.bytes
```

Notes: `retention.bytes` is **per partition**, not per topic. `-1` means unlimited.

### Lab: watch retention delete data (about 2 minutes)

```bash
kafka-topics.sh --bootstrap-server $BS --create --topic ret-test --partitions 1 --replication-factor 3 \
  --config retention.ms=20000 --config segment.ms=10000      # 20 s retention, roll a segment every 10 s

for i in 1 2 3 4 5; do echo "msg-$i"; done | \
  kafka-console-producer.sh --bootstrap-server $BS --topic ret-test

kafka-get-offsets.sh --bootstrap-server $BS --topic ret-test --time -2   # earliest -> ret-test:0:0
kafka-get-offsets.sh --bootstrap-server $BS --topic ret-test --time -1   # latest   -> ret-test:0:5
# wait ~90 s, repeat both. Earliest jumps to 5: the five messages are gone, offsets are NOT reused.
```

Peek at the disk (from your Mac): `kubectl --context devk8s-admin@devk8s -n kafka exec kafka-0 -- ls -la /var/lib/kafka/data/ret-test-0`.
You will see `*.log.deleted` files (removed for real after `file.delete.delay.ms`, 60 s).

Other cleanup mode: `cleanup.policy=compact` keeps the **latest record per key** forever (used for
"current state" topics, e.g. `__consumer_offsets`). `cleanup.policy=compact,delete` does both.

Inspect the raw log file:

```bash
# from the client pod you cannot see broker disks; run this on a broker instead
kubectl --context devk8s-admin@devk8s -n kafka exec kafka-0 -- \
  /opt/kafka/bin/kafka-dump-log.sh --print-data-log \
  --files /var/lib/kafka/data/orders-0/00000000000000000000.log
```

---

## 6. Resiliency labs (do these, they are the point of the lab)

Keep a "watcher" terminal running in the client pod throughout:

```bash
watch -n2 'kafka-topics.sh --bootstrap-server $BS --describe --topic orders'
```

Create `orders` first (section 2).

### Lab A: lose one broker (no downtime expected)

```bash
kubectl --context devk8s-admin@devk8s -n kafka delete pod kafka-2
```

What I observed within seconds:

```
Partition: 0  Leader: 1  Replicas: 1,2,0  Isr: 1,0        <- broker 2 dropped out of the ISR
Partition: 1  Leader: 0  Replicas: 2,0,1  Isr: 0,1        <- leader moved from 2 to 0 automatically
Partition: 2  Leader: 0  Replicas: 0,1,2  Isr: 0,1
```

* Producing with `acks=all` **still works** (2 in-sync replicas >= `min.insync.replicas=2`).
* StatefulSet recreates the pod, it reuses its PVC, catches up, and `Isr` returns to `1,0,2`.
* While partitions have fewer replicas than RF they show up in
  `kafka-topics.sh --bootstrap-server $BS --describe --under-replicated-partitions`.

### Lab B: lose two brokers (writes must be refused, nothing is lost)

```bash
kubectl --context devk8s-admin@devk8s -n kafka scale sts kafka --replicas=1   # only kafka-0 left
echo x | kafka-console-producer.sh --bootstrap-server kafka-0.kafka-headless.kafka.svc.cluster.local:9092 \
    --topic orders --producer-property acks=all
```

Observed: `Error: NOT_ENOUGH_REPLICAS`, and the message `x` was **not** stored. That is the safety net
working: Kafka prefers refusing a write over accepting one that exists on a single disk.

```bash
kubectl --context devk8s-admin@devk8s -n kafka scale sts kafka --replicas=3   # restore
```

All earlier messages were intact afterwards. Takeaways:
`acks=all` + `min.insync.replicas=2` + RF=3 = **durability over availability**. If you set
`min.insync.replicas=1` the write would be accepted, and you could lose it if that one broker died.
Also note that these nodes are controllers too: with only 1 of 3 left, the KRaft quorum has no majority,
so topic/leader changes are impossible until a second node returns.

> Only ever scale back to 3 here. Lab B is also why `PodDisruptionBudget` has `minAvailable: 2`:
> `kubectl drain` will refuse to evict a 2nd broker while the 1st is still recovering.

### Lab C: drain a worker node (planned maintenance; not yet run on this cluster)

```bash
# --force is needed because kafka-client is a bare pod (no controller to recreate it)
kubectl --context devk8s-admin@devk8s drain devk8s-workers-rhmwv-x7dkv --ignore-daemonsets --delete-emptydir-data --force
```

The broker pod there can't be rescheduled while the other two brokers sit on the other nodes
(pod anti-affinity forces one broker per node), so it stays `Pending` until you
`kubectl uncordon` the node. That is the correct, safe behaviour: you never get two brokers on one machine.
Note that `kafka-client` is also deleted if it is on that node; re-apply the manifest to bring it back.

### Lab D: consumer crash and rebalance

Run two consumers in group `billing` (section 4), kill one with Ctrl-C, and re-run
`kafka-consumer-groups.sh --describe --group billing`: its partitions move to the survivor and no
message is skipped (offsets are committed per partition).

### Lab E: load and watch lag

```bash
kafka-producer-perf-test.sh --topic orders --num-records 500000 --record-size 200 --throughput -1 \
  --producer-props bootstrap.servers=$BS acks=all
kafka-consumer-perf-test.sh --bootstrap-server $BS --topic orders --messages 500000 --group perf
```

Do Lab A *while* this runs and watch throughput dip and recover.

---

## 7. Troubleshooting platform

Work top-down: **Kubernetes first, then Kafka.**

### 7.1 Kubernetes layer

```bash
k get pods -o wide                         # all READY 1/1? on 3 different nodes?
k describe pod kafka-0                     # Events: scheduling, PVC, probes, OOMKilled?
k logs kafka-0 --tail=100                  # broker log
k logs kafka-0 --previous                  # log of the crashed container
k get pvc ; k get events --sort-by=.lastTimestamp | tail -20
k top pod                                  # needs metrics-server
k exec kafka-0 -- df -h /var/lib/kafka/data   # disk space on the broker
```

### 7.2 Kafka layer

| Question | Command |
|---|---|
| Are all brokers up? | `kafka-broker-api-versions.sh --bootstrap-server $BS \| grep -E '^kafka'` |
| Is controller quorum healthy? | `kafka-metadata-quorum.sh --bootstrap-server $BS describe --status` |
| Under-replicated partitions | `kafka-topics.sh --bootstrap-server $BS --describe --under-replicated-partitions` |
| Partitions below min ISR (writes failing) | `kafka-topics.sh --bootstrap-server $BS --describe --under-min-isr-partitions` |
| Partitions with no leader (unavailable) | `kafka-topics.sh --bootstrap-server $BS --describe --unavailable-partitions` |
| Consumer lag | `kafka-consumer-groups.sh --bootstrap-server $BS --describe --group <g>` |
| Disk used per topic | `kafka-log-dirs.sh --bootstrap-server $BS --describe --topic-list orders` |
| Topic config overrides | `kafka-configs.sh --bootstrap-server $BS --entity-type topics --entity-name orders --describe` |
| Effective broker config | `kafka-configs.sh --bootstrap-server $BS --entity-type brokers --entity-name 0 --describe --all` |
| Put leaders back on preferred brokers | `kafka-leader-election.sh --bootstrap-server $BS --election-type preferred --all-topic-partitions` |

### 7.3 Symptom -> cause table

| Symptom | Likely cause |
|---|---|
| Pod `CrashLoopBackOff`, log says `Found directory .../lost+found` | Volume root of an ext4 disk has `lost+found`. **We hit this while building the lab.** Fixed by `subPath: kafka` on the volume mount. |
| Client: `UnknownHostException: kafka-1...` | Client is outside the cluster, or DNS issue. Brokers advertise in-cluster names. |
| Producer: `NOT_ENOUGH_REPLICAS` | ISR < `min.insync.replicas`. A broker is down/slow. Check `--under-min-isr-partitions`. |
| Producer: `LEADER_NOT_AVAILABLE` | Leader election in progress (transient) or topic just created. Retry. If persistent: `--unavailable-partitions`. |
| Producer/consumer: `UNKNOWN_TOPIC_OR_PARTITION` | Topic doesn't exist and auto-create is off in this lab. Create it. |
| Producer hangs, then `TimeoutException` | No reachable leader, or controller quorum lost (2 of 3 nodes down). |
| Consumer lag keeps growing | Slow consumer, too few partitions (max useful consumers = partition count), or stuck rebalance. |
| Consumer gets nothing | Wrong topic, group already at the end (use a new group or `--from-beginning`), retention already deleted the data. |
| Broker pod `OOMKilled` (exit 137) | Memory limit (1.5 Gi) too low for heap (512 Mi) + page cache + metaspace. Raise the limit. |
| Broker disk full -> broker crashes | Retention too long for the 5 Gi PVC. Lower `retention.ms/bytes`; PVC can be expanded (`ceph-block` allows expansion). |
| Pod `Pending` | Anti-affinity: fewer than 3 schedulable nodes, or PVC can't attach. `k describe pod`. |
| `Isr` never returns to full after a restart | Follower is catching up (check `describe --replication`) or can't reach the leader on 9092. |

### 7.4 Useful logs to grep

```bash
k logs kafka-0 | grep -E "ERROR|WARN" | tail -30
k logs kafka-0 | grep -iE "ISR|Shrinking|Expanding"      # ISR changes
k logs kafka-0 | grep -iE "rebalance|GroupCoordinator"    # consumer group activity
```

Raise log verbosity at runtime without restart (example: replica fetcher):

```bash
kafka-configs.sh --bootstrap-server $BS --entity-type broker-loggers --entity-name 0 --alter \
  --add-config kafka.server.ReplicaFetcherThread=DEBUG
```

---

## 8. Things deliberately **not** in this lab (what you'd add in real life)

TLS/SASL authentication and ACLs, a rack/zone-aware `broker.rack`, separate dedicated controllers,
metrics (Prometheus JMX exporter) + alerting on under-replicated partitions and consumer lag,
a schema registry, Kafka Connect, an operator such as Strimzi, and an external listener.

## 9. Cleanup

```bash
kubectl --context devk8s-admin@devk8s delete namespace kafka   # removes everything incl. data (PVC reclaim policy = Delete)
```
