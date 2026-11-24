# Kafka Training

A self-contained, offline website that teaches Apache Kafka from zero to cluster administration,
using the real 3-broker Kafka cluster running in the `kafka` namespace of `devk8s`
(see [`../kafka`](../kafka) for the manifest and the earlier lab notes).

## Open it

No build step, no server, no internet needed:

```bash
open labs/kafka-training/index.html          # macOS
```

or serve it (useful if your browser restricts `file://` storage):

```bash
cd labs/kafka-training && python3 -m http.server 8765
# then browse to http://127.0.0.1:8765
```

Progress and quiz scores are kept in your browser's local storage (the sidebar has a reset button).

## The course

| # | Module | What you learn | Interactive |
|---|---|---|---|
| 1 | What is Kafka and why | The problem Kafka solves, events, Kafka vs queues, when not to use it | hub vs direct-calls simulator, life-of-a-message walkthrough |
| 2 | Core building blocks | Record, topic, partition, offset, broker, cluster; ordering rules | record anatomy explorer, key-to-partition log explorer (real Kafka hashing) |
| 3 | Publishing data | Produce path, partitioner, acks, retries, idempotence, batching, code in Java/Python/Go | produce-path stepper, "can this message be lost?" simulator, batching model |
| 4 | Replication and failure | Leader/follower, ISR, min.insync.replicas, high watermark, unclean election | guided broker-failure walkthrough, full 3-broker cluster simulator, survivability calculator |
| 5 | Consuming data | Consumer groups, offsets, lag, replay, delivery semantics, rebalancing, code | consumer-group simulator with lag and rebalance, delivery-guarantee crash demo |
| 6 | Inside the cluster | Segments, retention, compaction, why Kafka is fast, KRaft quorum, disk sizing | segment/retention simulator, compaction simulator, quorum simulator, disk calculator |
| 7 | Kafka on your Kubernetes | Every object in the deployment and why; how clients connect; failure behaviour | clickable architecture, connection walkthrough |
| 8 | Hands-on CLI lab | 12 labs with real commands and the real output from this cluster | checklist progress |
| 9 | Administration and troubleshooting | Health check, safe rolling restart, disk growth, runbooks | CLI command builder, 12-symptom troubleshooter |
| 10 | Mastery | Final exam, command cheat sheet, glossary, capstone challenges | searchable cheat sheet and glossary |

Recommended path: modules 1 to 6 for the concepts (about 2.5 hours), then 7 to 9 at the keyboard
against the cluster (about 2 hours).

## Accuracy notes

* Blocks labelled **Real output** were captured from this cluster (Apache Kafka 4.0.0, KRaft, 3 brokers).
* The key-to-partition simulator implements Kafka's default `murmur2` partitioner and was checked
  against 12 keys on the live cluster (all matched).
* The replication, consumer-group, segment and quorum simulators are **simplified models** that follow
  Kafka's rules (ISR membership, min ISR, clean leader election, partition-per-consumer assignment,
  segment rolling and retention) but not its timing. Treat them as teaching aids, then confirm on the cluster
  using module 8.
* Procedures not executed on this cluster are labelled as such (for example growing the PVCs and the
  drain lab). Everything else in modules 8 and 9 was run.

## Files

```
index.html                  course home
modules/01..10-*.html       the modules
assets/css/style.css        design system (light and dark)
assets/js/theme.js          applies the saved/preferred theme before first paint
assets/js/app.js            navigation, progress, quiz and stepper components, Kafka partitioner
```
