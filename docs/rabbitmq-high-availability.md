# RabbitMQ high availability

RabbitMQ carries two things: the jobs that deliver sign-in codes, and the currency events that the WebSocket gateways broadcast. With one broker node, a restart or a lost node stopped sign-in, and a server that started while the broker was away crash-looped. This document describes how to run RabbitMQ without a single point of failure, and how the server behaves while the broker, or one of its nodes, is away ([#232](https://github.com/mdastpak/Crypto-Exchange/issues/232)).

## Deployment

Run a cluster of three nodes on three hosts, and list them in `rabbitmq.hosts` ([configuration](configuration.md#rabbitmq)):

```yaml
rabbitmq:
  hosts: ["rmq-1.internal:5671", "rmq-2.internal:5671", "rmq-3.internal:5671"]
  tls:
    enabled: true
```

The server dials the nodes in random order, so that the instances spread over the cluster, and uses the first that answers. When its connection closes, it dials them again, from a new random order. A load balancer in front of the cluster works too, with `host` and `port`, but then the server cannot skip a node that accepts connections and does not work.

With TLS, each connection checks the name of the node it dials, unless `tls.server_name` names one for all of them, so the certificate of each node must carry its name in `hosts`.

On the RabbitMQ side, the cluster needs at least three nodes so that a quorum queue keeps a majority while one is down. Use the same Erlang cookie on every node, and `cluster_partition_handling = pause_minority`, so that a node cut off from the others stops instead of accepting work that the majority does not see.

## The queues

| Queue | Type | What a lost node does |
| --- | --- | --- |
| `notifications.otp.quorum` | Quorum | Nothing. The queue lives on every node and needs a majority, two of three. A job that the broker confirmed is on the disks of a majority, so it survives the loss of any one node. |
| `notifications.otp.dead-letter.quorum` | Quorum | Nothing, as above |
| The WebSocket queue of each instance | Classic, exclusive, server-named | It lives on the node that the instance is connected to, and goes away with that node or that connection. The instance declares a new one on another node and tells its clients to reload ([below](#websocket-clients)). |

A quorum queue hands out a job at most ten times (`x-delivery-limit`), so that a job that crashes its worker every time cannot circle forever. It has no dead-letter exchange in the broker: that would copy the code into the dead-letter queue. A job that is given up is recorded by the worker instead, without the code or the raw contact ([configuration](configuration.md#notifications)).

### Sent once

A worker that sends a code and stops before it acknowledges the job, because its node or its process went away, leaves the job unacknowledged, and the broker hands it out again. The worker therefore records every job that it delivered in Redis (`notify:otp:delivered:<id>`), until a minute after the code expires, and acknowledges a job that it finds there without sending it again. When Redis cannot be read, the job is sent: a code sent twice is better than none. Each job carries an ID, its message ID, for this; a job of an earlier version is recognized by the hash of its body.

### Upgrading from the classic queues

Versions before #232 used the classic queues `notifications.otp` and `notifications.otp.dead-letter`. A queue cannot change its type in place, so the quorum queues have new names, and the upgrade needs no step by hand:

1. A new instance declares the quorum queues and binds `notifications.otp.quorum` to the exchange.
2. It unbinds `notifications.otp`, so that new jobs go only to the quorum queue, and delivers the jobs left in it.
3. It deletes `notifications.otp` once it is empty and no instance of an earlier version consumes it. Until then it looks again every 30 seconds, since an instance of an earlier version that reconnects binds the queue again.

Delete `notifications.otp.dead-letter` by hand once its records have expired, seven days after the upgrade.

## While the broker is away

The server starts without the broker and connects in the background, so a rolling deploy during a broker outage does not crash-loop. While no node can be reached:

| Feature | Behavior |
| --- | --- |
| Readiness (`/system/ready`) | `200` with the status `degraded` and `rabbitmq` `error`. The instance stays in service, since every instance shares the broker: taking all of them out would turn a broker outage into an outage of the API. |
| A sign-in code request | `503 delivery_unavailable` with `Retry-After: 5`, within the request, instead of a code that is never sent ([API](api.md#errors)) |
| Everything else of sign-in, sessions, the REST routes | Unaffected |
| Currency events | They wait in the outbox in PostgreSQL ([#89](https://github.com/mdastpak/Crypto-Exchange/issues/89)) and are published once the broker is back |
| WebSocket clients | Stay connected and receive no currency events until the broker is back, then `currencies.resync` |

When one node of a cluster is down, the server moves to another one within about a second; the requests of that second may get `503 delivery_unavailable`.

### WebSocket clients

An instance whose subscription to the currency events was lost subscribes again on whichever node it reaches. The events published in between are lost for its clients, so it then sends every client `{"event": "currencies.resync", "server_time": "..."}`, and a client reloads the currencies over REST ([API](api.md#server-messages)). The sequence numbers of the channel protocol ([#66](https://github.com/mdastpak/Crypto-Exchange/issues/66)) will let a client fetch only what it missed.

## The failover drill

`scripts/rabbitmq-ha/failover-drill.sh` starts a cluster of three nodes on Docker and runs the `TestHA` tests of `internal/notify` against it. They kill each node in turn, starting with the one that the server uses, and check that:

- the server moves to another node, and codes are queued again within a second of the kill;
- every code queued before and during the outage is delivered, and each one exactly once;
- the node comes back and rejoins the cluster.

The **RabbitMQ failover drill** workflow runs it on the pull requests that change the RabbitMQ or delivery code, every month, and on demand. Run it locally from the repository root with Docker and Go.
