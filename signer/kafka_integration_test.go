package signer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/j0sh/minikafka"
	"github.com/j0sh/minikafka/storage/memory"
	"github.com/livepeer/node/pm/wire"
	"github.com/stretchr/testify/require"
)

func TestRealKafkaDeliveryAndRestartReplay(t *testing.T) {
	for _, test := range []struct {
		name       string
		tls        bool
		authMethod string
	}{
		{name: "plaintext"},
		{name: "tls", tls: true},
		{name: "plain", authMethod: "plain"},
		{name: "plain-tls", tls: true, authMethod: "plain"},
		{name: "scram-sha-512", authMethod: "scram-sha-512"},
		{name: "scram-sha-512-tls", tls: true, authMethod: "scram-sha-512"},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := memory.Open()
			require.NoError(t, backend.CreateTopic(t.Context(), "signing", minikafka.TopicOptions{}))
			brokerConfig := minikafka.Config{Addr: "127.0.0.1:0", Store: backend}
			if test.authMethod != "" {
				mechanism := minikafka.SASLSCRAMSHA512
				if test.authMethod == "plain" {
					mechanism = minikafka.SASLPlain
				}
				brokerConfig.SASL = &minikafka.SASLConfig{Mechanisms: []minikafka.SASLMechanism{mechanism}, Users: map[string]string{"signer": "password"}}
			}
			broker, err := minikafka.Open(brokerConfig)
			require.NoError(t, err)
			brokerCtx, brokerStop := context.WithCancel(t.Context())
			brokerDone := make(chan error, 1)
			go func() { brokerDone <- broker.Serve(brokerCtx) }()
			t.Cleanup(func() { brokerStop(); require.NoError(t, broker.Close()); require.NoError(t, <-brokerDone) })
			scheme := "kafka://"
			if test.tls {
				scheme = "" // A bare broker address defaults to TLS.
			}
			config := KafkaConfig{Broker: kafkaBrokerURL(t, scheme+broker.Addr()), Topic: "signing", AuthMethod: test.authMethod, OutboxDB: filepath.Join(t.TempDir(), "outbox.sqlite"), OutboxMaxBytes: 1 << 20}
			var proxyAddress string
			var roots *x509.CertPool
			if test.authMethod != "" {
				config.Username, config.Password = "signer", "password"
			}
			if test.tls {
				proxyAddress, roots = tlsKafkaProxy(t, broker.Addr())
			}
			open := func() *kafkaProducer {
				p, err := openKafkaProducer(t.Context(), &config, "payer")
				require.NoError(t, err)
				if test.tls {
					p.transport.TLS.RootCAs = roots
					// MiniKafka advertises its plain listener. Route all test
					// connections through a TLS terminator, including metadata leaders.
					p.transport.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, network, proxyAddress)
					}
				}
				return p
			}
			p := open()
			s, info := testService(t)
			s.events = p
			require.Equal(t, 200, postPayment(t, s, map[string]any{"type": "fixed", "app": "test-app", "orchestrator": wire.EncodeOrchestratorInfo(info)}).Code)
			original, err := p.outbox.pending(t.Context())
			require.NoError(t, err)
			require.Len(t, original, 1)
			if test.authMethod != "" {
				badConfig := config
				badConfig.Password = "incorrect"
				badTransport, err := badConfig.transport()
				require.NoError(t, err)
				p.transport.SASL = badTransport.SASL
				_, err = p.publish(t.Context())
				require.Error(t, err)
				require.True(t, p.ready(t.Context()), "Kafka authentication failure must retain the event without blocking storage readiness")
				require.NoError(t, p.Close())
				p = open()
			}
			// Kafka accepts the event, but local acknowledgement fails.
			_, err = p.outbox.db.Exec(`CREATE TRIGGER fail_ack BEFORE DELETE ON signer_kafka_events BEGIN SELECT RAISE(FAIL,'injected failure'); END`)
			require.NoError(t, err)
			_, err = p.publish(t.Context())
			require.Error(t, err)
			_, err = p.outbox.db.Exec(`DROP TRIGGER fail_ack`)
			require.NoError(t, err)
			require.NoError(t, p.Close())
			p = open()
			defer func() { require.NoError(t, p.Close()) }()
			recovered, err := p.outbox.pending(t.Context())
			require.NoError(t, err)
			require.Equal(t, original, recovered)
			progress, err := p.publish(t.Context())
			require.NoError(t, err)
			require.True(t, progress)
			stats, err := p.outbox.stats(t.Context())
			require.NoError(t, err)
			require.Zero(t, stats.Count)
			records, err := backend.Fetch(t.Context(), minikafka.FetchRequest{Topic: "signing", Partition: 0, Offset: 0})
			require.NoError(t, err)
			require.Len(t, records.Records, 2)
			for _, record := range records.Records {
				require.Equal(t, []byte(original[0].ID), record.Key)
				require.Equal(t, original[0].Payload, record.Value)
			}
			var event signingEvent
			require.NoError(t, json.Unmarshal(records.Records[0].Value, &event))
			require.Equal(t, "10", event.Data.ComputedFee)
			artifact := filepath.Join(t.ArtifactDir(), "signing-event.json")
			require.NoError(t, os.WriteFile(artifact, records.Records[0].Value, 0600))
			t.Logf("delivered accounting fixture: %s", artifact)
		})
	}
}

func tlsKafkaProxy(t *testing.T, upstream string) (string, *x509.CertPool) {
	t.Helper()
	certificateServer := httptest.NewTLSServer(http.NotFoundHandler())
	certificates := certificateServer.TLS.Certificates
	roots := x509.NewCertPool()
	roots.AddCert(certificateServer.Certificate())
	certificateServer.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: certificates, MinVersion: tls.VersionTLS12})
	require.NoError(t, err)
	var connections sync.Map
	var workers sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Store(client, true)
			workers.Go(func() {
				defer func() { connections.Delete(client); _ = client.Close() }()
				server, err := net.DialTimeout("tcp", upstream, time.Second)
				if err != nil {
					return
				}
				defer func() { _ = server.Close() }()
				copyDone := make(chan struct{})
				go func() { _, _ = io.Copy(server, client); _ = server.Close(); close(copyDone) }()
				_, _ = io.Copy(client, server)
				_ = client.Close()
				<-copyDone
			})
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-acceptDone
		connections.Range(func(key, _ any) bool { _ = key.(net.Conn).Close(); return true })
		workers.Wait()
	})
	return listener.Addr().String(), roots
}

func TestKafkaUnresponsiveBrokerShutdownRetainsOutbox(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			accepted <- connection
		}
	}()
	p, err := openKafkaProducer(t.Context(), &KafkaConfig{Broker: kafkaBrokerURL(t, "kafka://"+listener.Addr().String()), Topic: "signing", OutboxDB: filepath.Join(t.TempDir(), "events.sqlite"), OutboxMaxBytes: 1 << 20}, "payer")
	require.NoError(t, err)
	require.NoError(t, p.Enqueue(t.Context(), testEvent()))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { p.run(ctx); _ = p.writer.Close(); close(done) }()
	var connection net.Conn
	select {
	case connection = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("producer did not connect")
	}
	defer func() { _ = connection.Close() }()
	cancel()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("Kafka shutdown exceeded bounded timeouts")
	}
	stats, err := p.outbox.stats(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), stats.Count)
	require.NoError(t, p.Close())
}
