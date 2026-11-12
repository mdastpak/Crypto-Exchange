package notify

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestHA tests run against a three-node RabbitMQ cluster, which
// scripts/rabbitmq-ha/failover-drill.sh starts on Docker (#232). They kill
// and restart its nodes, so they run only when the drill sets
// RABBITMQ_HA_HOSTS (the AMQP addresses of the nodes) and
// RABBITMQ_HA_CONTAINERS (their container names, in the same order).

type haCluster struct {
	hosts      []string
	containers []string
}

func newHACluster(t *testing.T) *haCluster {
	t.Helper()
	hosts, containers := os.Getenv("RABBITMQ_HA_HOSTS"), os.Getenv("RABBITMQ_HA_CONTAINERS")
	if hosts == "" || containers == "" {
		t.Skip("RABBITMQ_HA_HOSTS and RABBITMQ_HA_CONTAINERS are not set; run scripts/rabbitmq-ha/failover-drill.sh")
	}
	c := &haCluster{hosts: strings.Split(hosts, ","), containers: strings.Split(containers, ",")}
	require.Len(t, c.containers, len(c.hosts), "one container per host")
	return c
}

func (c *haCluster) docker(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	require.NoError(t, err, "docker %s: %s", strings.Join(args, " "), out)
}

// kill stops the node at host i at once, as a crash would.
func (c *haCluster) kill(t *testing.T, i int) {
	t.Helper()
	c.docker(t, "kill", c.containers[i])
}

// restart starts the node at host i again and waits until it runs and has
// rejoined the cluster.
func (c *haCluster) restart(t *testing.T, i int) {
	t.Helper()
	c.docker(t, "start", c.containers[i])
	require.Eventually(t, func() bool {
		return exec.Command("docker", "exec", c.containers[i], "rabbitmq-diagnostics", "-q", "check_running").Run() == nil
	}, 2*time.Minute, time.Second, "%s did not come back", c.containers[i])
}

// indexOf returns the index of host in the cluster's hosts.
func (c *haCluster) indexOf(t *testing.T, host string) int {
	t.Helper()
	for i, h := range c.hosts {
		if h == host {
			return i
		}
	}
	t.Fatalf("connected to %q, which is not a node of the cluster %v", host, c.hosts)
	return -1
}

// configure points the configuration at every node of the cluster.
func (c *haCluster) configure(t *testing.T) {
	t.Helper()
	saved := config.Config.RabbitMQ
	t.Cleanup(func() { config.Config.RabbitMQ = saved })
	config.Config.RabbitMQ.Host, config.Config.RabbitMQ.Port = "", ""
	config.Config.RabbitMQ.Hosts = c.hosts
	config.Config.RabbitMQ.Username = envOr("RABBITMQ_HA_USER", "guest")
	config.Config.RabbitMQ.Password = envOr("RABBITMQ_HA_PASSWORD", "guest")
	config.Config.RabbitMQ.VHost = ""
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// recorder counts what a channel delivered, by the code of each message.
type recorder struct {
	mu   sync.Mutex
	sent map[string]int
}

func (r *recorder) Deliver(_ context.Context, msg OTPMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent[msg.Code]++
	return nil
}

func (r *recorder) count(code string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent[code]
}

// sendUntilQueued queues msg, retrying as a client retries an answer of
// 503 delivery_unavailable, and fails when it is not queued within 30
// seconds.
func sendUntilQueued(t *testing.T, sender *QueueSender, msg OTPMessage) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := sender.SendOTP(ctx, msg)
		cancel()
		if err == nil {
			return
		}
		require.True(t, time.Now().Before(deadline), "code %s was not queued within 30 seconds: %v", msg.Code, err)
		time.Sleep(200 * time.Millisecond)
	}
}

// TestHA_DeliversWhileANodeIsDown kills each node of the cluster in turn,
// starting with the one that the server is connected to. While a node is
// down, the server reconnects to another one, codes are queued within
// seconds, and each one is delivered exactly once. The quorum queues keep
// the codes queued before a kill, since two of the three nodes still hold
// them.
func TestHA_DeliversWhileANodeIsDown(t *testing.T) {
	cluster := newHACluster(t)
	cluster.configure(t)
	logs := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("worker log:\n%s", logs.String())
		}
	})

	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	opts := WorkerOptions{
		Exchange:           "ha_notifications_" + id,
		Queue:              "ha_otp_" + id,
		DeadLetterExchange: "ha_dead_letter_" + id,
		DeadLetterQueue:    "ha_otp_dead_letter_" + id,
		Concurrency:        2,
		Delivered:          &memoryLog{},
		Logger:             log.New(logs, "", 0),
	}
	svc, err := rabbitmq.NewRabbitMQService(&config.Config, opts.Exchange)
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.True(t, svc.IsHealthy(), "connected to the cluster: %v", svc.LastError())

	rec := &recorder{sent: map[string]int{}}
	channels := Channels{Email: rec}
	worker := NewWorker(svc, channels, opts)
	require.NoError(t, worker.Start())
	defer stopWorker(t, worker)
	sender := NewQueueSender(svc, opts.Exchange, channels, time.Minute)

	var codes []string
	send := func(round string, n int) {
		for i := 0; i < n; i++ {
			msg := testEmailMessage
			msg.Code = fmt.Sprintf("%s-%02d", round, i)
			sendUntilQueued(t, sender, msg)
			codes = append(codes, msg.Code)
		}
	}
	allDelivered := func() bool {
		for _, code := range codes {
			if rec.count(code) == 0 {
				return false
			}
		}
		return true
	}

	send("before", 5)
	killed := map[int]bool{}
	for round := 0; round < len(cluster.hosts); round++ {
		// The node that the server uses first, then the others.
		victim := cluster.indexOf(t, svc.ConnectedTo())
		if killed[victim] {
			for i := range cluster.hosts {
				if !killed[i] {
					victim = i
					break
				}
			}
		}
		killed[victim] = true
		t.Logf("round %d: killing %s (%s)", round, cluster.containers[victim], cluster.hosts[victim])
		// Codes still waiting in the queue are kept by the other nodes.
		send(fmt.Sprintf("queued%d", round), 5)
		cluster.kill(t, victim)

		started := time.Now()
		send(fmt.Sprintf("down%d", round), 10)
		t.Logf("round %d: queued 10 codes %s after the kill, connected to %s", round, time.Since(started).Round(time.Millisecond), svc.ConnectedTo())
		assert.NotEqual(t, cluster.hosts[victim], svc.ConnectedTo(), "the server moved to another node")
		require.Eventually(t, allDelivered, time.Minute, 100*time.Millisecond, "every code queued so far is delivered while %s is down", cluster.containers[victim])

		cluster.restart(t, victim)
	}

	send("after", 5)
	require.Eventually(t, allDelivered, time.Minute, 100*time.Millisecond)
	for _, code := range codes {
		assert.Equal(t, 1, rec.count(code), "code %s is delivered exactly once", code)
	}
}
